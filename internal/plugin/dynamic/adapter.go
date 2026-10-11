package dynamic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/gogf/gf/v2/encoding/gjson"

	"github.com/JimDevil/ext-proc/internal/plugin"
	extproc "github.com/JimDevil/ext-proc/sdk"
)

// subprocessPlugin implements plugin.Plugin by forwarding every phase call to
// a plugin subprocess over the ext_proc protocol. Each call uses one
// short-lived stream carrying the FULL current phase state; the returned
// mutations are applied to the host-side Context. Stateless per phase —
// nothing to leak between streams, and a crashed subprocess is detected at
// the next call (the process manager revives it).
type subprocessPlugin struct {
	name      string
	proc      *managedProcess
	timeout   time.Duration
	logger    *slog.Logger
	closeOnce sync.Once
}

func (s *subprocessPlugin) Name() string { return s.name }

// Init is a no-op: the subprocess ran the real plugin's Init at launch time
// (with this instance's merged config node).
func (s *subprocessPlugin) Init(*gjson.Json, *slog.Logger) error { return nil }

// Close shuts the subprocess down. Idempotent: a retired chain and the
// graceful-shutdown path may both call it.
func (s *subprocessPlugin) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.proc.Close() })
	return err
}

func (s *subprocessPlugin) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{
			Headers:     toHeaderMap(headers),
			EndOfStream: ctx.EndOfStream(),
		},
	}})
}

func (s *subprocessPlugin) OnRequestBody(ctx plugin.Context, body []byte) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestBody{
		RequestBody: &epb.HttpBody{Body: body, EndOfStream: ctx.EndOfStream()},
	}})
}

func (s *subprocessPlugin) OnRequestTrailers(ctx plugin.Context, trailers extproc.AllHeaders) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestTrailers{
		RequestTrailers: &epb.HttpTrailers{Trailers: toHeaderMap(trailers)},
	}})
}

func (s *subprocessPlugin) OnResponseHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_ResponseHeaders{
		ResponseHeaders: &epb.HttpHeaders{
			Headers:     toHeaderMap(headers),
			EndOfStream: ctx.EndOfStream(),
		},
	}})
}

func (s *subprocessPlugin) OnResponseBody(ctx plugin.Context, body []byte) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_ResponseBody{
		ResponseBody: &epb.HttpBody{Body: body, EndOfStream: ctx.EndOfStream()},
	}})
}

func (s *subprocessPlugin) OnResponseTrailers(ctx plugin.Context, trailers extproc.AllHeaders) error {
	return s.call(ctx, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_ResponseTrailers{
		ResponseTrailers: &epb.HttpTrailers{Trailers: toHeaderMap(trailers)},
	}})
}

// call sends one phase message and applies the single response.
func (s *subprocessPlugin) call(ctx plugin.Context, req *epb.ProcessingRequest) error {
	cli, err := s.proc.externalClient()
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	st, err := cli.Process(callCtx)
	if err != nil {
		return fmt.Errorf("plugin %s: open ext_proc stream: %w", s.name, err)
	}
	if err := st.Send(req); err != nil {
		return fmt.Errorf("plugin %s: send phase request: %w", s.name, err)
	}
	resp, err := st.Recv()
	if err != nil {
		return fmt.Errorf("plugin %s: receive phase response: %w", s.name, err)
	}
	return applyResponse(ctx, resp)
}

// applyResponse folds one ProcessingResponse into the host-side Context:
// ImmediateResponse short-circuits (ctx.Aborted() then stops the chain);
// CommonResponse header/body mutations are replayed through the Context API.
func applyResponse(ctx plugin.Context, resp *epb.ProcessingResponse) error {
	switch r := resp.GetResponse().(type) {
	case *epb.ProcessingResponse_ImmediateResponse:
		imm := r.ImmediateResponse
		hdrs := map[string]extproc.HeaderValue{}
		for _, hvo := range imm.GetHeaders().GetSetHeaders() {
			h := hvo.GetHeader()
			hdrs[strings.ToLower(h.GetKey())] = extproc.HeaderValue{RawValue: rawOf(h)}
		}
		return ctx.CancelRequest(int32(imm.GetStatus().GetCode()), hdrs, string(imm.GetBody()))

	case *epb.ProcessingResponse_RequestHeaders:
		return applyCommon(ctx, r.RequestHeaders.GetResponse())
	case *epb.ProcessingResponse_RequestBody:
		return applyCommon(ctx, r.RequestBody.GetResponse())
	case *epb.ProcessingResponse_ResponseHeaders:
		return applyCommon(ctx, r.ResponseHeaders.GetResponse())
	case *epb.ProcessingResponse_ResponseBody:
		return applyCommon(ctx, r.ResponseBody.GetResponse())

	case *epb.ProcessingResponse_RequestTrailers:
		return applyHeaderMutation(ctx, r.RequestTrailers.GetHeaderMutation())
	case *epb.ProcessingResponse_ResponseTrailers:
		return applyHeaderMutation(ctx, r.ResponseTrailers.GetHeaderMutation())

	default:
		return nil // empty continue
	}
}

// applyCommon replays a CommonResponse's mutations onto the host Context.
func applyCommon(ctx plugin.Context, cr *epb.CommonResponse) error {
	if cr == nil {
		return nil
	}
	if err := applyHeaderMutation(ctx, cr.GetHeaderMutation()); err != nil {
		return err
	}
	if bm := cr.GetBodyMutation(); bm != nil {
		switch m := bm.GetMutation().(type) {
		case *epb.BodyMutation_Body:
			if err := ctx.ReplaceBody(m.Body); err != nil {
				return err
			}
		default:
			// Streamed body mutations are not carried across the wire yet —
			// dynamic plugins run under BUFFERED body semantics today.
			return fmt.Errorf("unsupported body mutation %T from subprocess plugin", m)
		}
	}
	return nil
}

// applyHeaderMutation replays set/remove header operations. The subprocess
// runs the same Envoy protection rules the host does, so rejected
// (:method/:authority/host/x-envoy-*) mutations cannot originate there.
func applyHeaderMutation(ctx plugin.Context, hm *epb.HeaderMutation) error {
	if hm == nil {
		return nil
	}
	for _, name := range hm.GetRemoveHeaders() {
		if err := ctx.RemoveHeader(name); err != nil {
			return err
		}
	}
	for _, hvo := range hm.GetSetHeaders() {
		h := hvo.GetHeader()
		hv := extproc.HeaderValue{RawValue: rawOf(h)}
		var err error
		switch hvo.GetAppendAction() {
		case corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD:
			err = ctx.AppendHeader(h.GetKey(), hv)
		default:
			// OVERWRITE_IF_EXISTS_OR_ADD / OVERWRITE_IF_EXISTS map to
			// OverwriteHeader; ADD_IF_ABSENT has no Context equivalent (the
			// interface cannot read headers) and degrades to overwrite.
			err = ctx.OverwriteHeader(h.GetKey(), hv)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// toHeaderMap converts AllHeaders (including pseudo headers, which the
// subprocess needs for matchRules/authority) into an Envoy HeaderMap.
func toHeaderMap(h extproc.AllHeaders) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for name, raws := range h.RawHeaders {
		for _, raw := range raws {
			hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: name, RawValue: raw})
		}
	}
	return hm
}

// rawOf picks the raw representation of a proto header value.
func rawOf(h *corev3.HeaderValue) []byte {
	if len(h.GetRawValue()) > 0 {
		return h.GetRawValue()
	}
	return []byte(h.GetValue())
}
