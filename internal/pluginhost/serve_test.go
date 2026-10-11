package pluginhost

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/JimDevil/ext-proc/internal/plugin"
	extproc "github.com/JimDevil/ext-proc/sdk"
)

// stubPlugin covers the wire paths the host adapter relies on: header
// mutation, body replacement and short-circuit.
type stubPlugin struct {
	plugin.BasePlugin
	value  string
	reject int32
}

func (p *stubPlugin) Name() string { return "stub" }

func (p *stubPlugin) OnRequestHeaders(ctx plugin.Context, headers extproc.AllHeaders) error {
	if p.reject != 0 {
		return ctx.CancelRequest(p.reject, map[string]extproc.HeaderValue{"x-reason": {Value: "nope"}}, "denied")
	}
	return ctx.OverwriteHeader("x-mutated", extproc.HeaderValue{Value: p.value})
}

func (p *stubPlugin) OnRequestBody(ctx plugin.Context, body []byte) error {
	return ctx.ReplaceBody([]byte("replaced:" + string(body)))
}

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// startTestServer serves one stub plugin on an ephemeral TCP listener.
func startTestServer(t *testing.T, p plugin.Plugin) (target string, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := extproc.AssembleExtProcServer(extproc.NewDefaultServerOptions(), newSingleProcessor("stub", p, testLogger), testLogger)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), srv.Stop
}

// phaseClient opens one stream and returns the response to req.
func phaseClient(t *testing.T, target string, req *epb.ProcessingRequest) *epb.ProcessingResponse {
	t.Helper()
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	st, err := epb.NewExternalProcessorClient(conn).Process(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := st.Send(req); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := st.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return resp
}

func headerMap(pairs ...string) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for i := 0; i+1 < len(pairs); i += 2 {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: pairs[i], RawValue: []byte(pairs[i+1])})
	}
	return hm
}

func TestSingleProcessorMutatesHeaders(t *testing.T) {
	t.Parallel()
	target, stop := startTestServer(t, &stubPlugin{value: "v1"})
	defer stop()

	resp := phaseClient(t, target, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{Headers: headerMap(":path", "/x", "x-old", "1")},
	}})

	mut := resp.GetRequestHeaders().GetResponse().GetHeaderMutation()
	if mut == nil || len(mut.GetSetHeaders()) != 1 {
		t.Fatalf("expected one set_headers mutation, got %+v", mut)
	}
	h := mut.GetSetHeaders()[0].GetHeader()
	if h.GetKey() != "x-mutated" || string(rawOfProto(h)) != "v1" {
		t.Fatalf("unexpected mutation: %s=%s", h.GetKey(), h.GetRawValue())
	}
}

func TestSingleProcessorImmediateResponse(t *testing.T) {
	t.Parallel()
	target, stop := startTestServer(t, &stubPlugin{reject: 403})
	defer stop()

	resp := phaseClient(t, target, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{Headers: headerMap(":path", "/x")},
	}})

	imm := resp.GetImmediateResponse()
	if imm == nil {
		t.Fatalf("expected immediate response, got %+v", resp.GetResponse())
	}
	if int32(imm.GetStatus().GetCode()) != 403 || string(imm.GetBody()) != "denied" {
		t.Fatalf("unexpected immediate response: %d %q", imm.GetStatus().GetCode(), imm.GetBody())
	}
	if reason := imm.GetHeaders().GetSetHeaders(); len(reason) != 1 || reason[0].GetHeader().GetKey() != "x-reason" {
		t.Fatalf("expected x-reason header on the immediate response: %+v", reason)
	}
}

func TestSingleProcessorReplacesBody(t *testing.T) {
	t.Parallel()
	target, stop := startTestServer(t, &stubPlugin{value: "v1"})
	defer stop()

	resp := phaseClient(t, target, &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestBody{
		RequestBody: &epb.HttpBody{Body: []byte("abc"), EndOfStream: true},
	}})

	bm := resp.GetRequestBody().GetResponse().GetBodyMutation()
	if bm == nil || string(bm.GetBody()) != "replaced:abc" {
		t.Fatalf("expected replaced body, got %+v", bm)
	}
}

func TestReadHandshake(t *testing.T) {
	t.Parallel()

	sc := bufio.NewScanner(strings.NewReader("noise line\nEXTPROC_PLUGIN_V1\ttcp://127.0.0.1:54321\n"))
	addr, err := ReadHandshake(sc, time.Second)
	if err != nil || addr != "tcp://127.0.0.1:54321" {
		t.Fatalf("handshake mismatch: %q, %v", addr, err)
	}

	// EOF without a handshake must error, not hang.
	sc = bufio.NewScanner(strings.NewReader("nothing here\n"))
	if _, err := ReadHandshake(sc, time.Second); err == nil {
		t.Fatal("expected an error on EOF without handshake")
	}
}

// rawOfProto mirrors the header raw/value fallback on the wire side.
func rawOfProto(h *corev3.HeaderValue) []byte {
	if len(h.GetRawValue()) > 0 {
		return h.GetRawValue()
	}
	return []byte(h.GetValue())
}
