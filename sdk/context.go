// Package extproc is the in-repo ext_proc server SDK (forked from
// wrossmorrow/envoy-extproc-sdk-go and refactored for production use;
// maintained in-tree from now on).
//
// Improvements over the original SDK (see sdk/README.md):
//   - panic recovery built into the Process loop: a handler panic no longer
//     takes down the whole process;
//   - new metric extproc_panic_recoveries_total;
//   - the public API is kept compatible with the original, so migrating is
//     just an import-path change.
//
// This file: the six phase constants, the per-stream request context
// (RequestContext) and its full mutation API.
package extproc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// Phase constants, one per ProcessingRequest kind in the ext_proc protocol.
const (
	REQUEST_PHASE_UNDETERMINED      = 0
	REQUEST_PHASE_REQUEST_HEADERS   = 1
	REQUEST_PHASE_REQUEST_BODY      = 2
	REQUEST_PHASE_REQUEST_TRAILERS  = 3
	REQUEST_PHASE_RESPONSE_HEADERS  = 4
	REQUEST_PHASE_RESPONSE_BODY     = 5
	REQUEST_PHASE_RESPONSE_TRAILERS = 6
)

// defaultPhaseIfInvalid collapses invalid phase values to UNDETERMINED so that
// downstream switches stay safe.
func defaultPhaseIfInvalid(phase int) int {
	switch phase {
	case REQUEST_PHASE_UNDETERMINED, REQUEST_PHASE_REQUEST_HEADERS, REQUEST_PHASE_REQUEST_BODY, REQUEST_PHASE_REQUEST_TRAILERS, REQUEST_PHASE_RESPONSE_HEADERS, REQUEST_PHASE_RESPONSE_BODY, REQUEST_PHASE_RESPONSE_TRAILERS:
		return phase
	default:
		return REQUEST_PHASE_UNDETERMINED
	}
}

// RequestPhaseToString returns a readable phase name for logs and metrics.
func RequestPhaseToString(phase int) string {
	switch phase {
	case REQUEST_PHASE_UNDETERMINED:
		return "UNDETERMINED"
	case REQUEST_PHASE_REQUEST_HEADERS:
		return "REQUEST_HEADERS"
	case REQUEST_PHASE_REQUEST_BODY:
		return "REQUEST_BODY"
	case REQUEST_PHASE_REQUEST_TRAILERS:
		return "REQUEST_TRAILERS"
	case REQUEST_PHASE_RESPONSE_HEADERS:
		return "RESPONSE_HEADERS"
	case REQUEST_PHASE_RESPONSE_BODY:
		return "RESPONSE_BODY"
	case REQUEST_PHASE_RESPONSE_TRAILERS:
		return "RESPONSE_TRAILERS"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", phase)
	}
}

// PhaseResponse is the per-phase response being accumulated:
// header/body mutations collect incrementally; an immediateResponse, once set,
// short-circuits the request.
type PhaseResponse struct {
	headerMutation    *extprocv3.HeaderMutation    // any phase
	bodyMutation      *extprocv3.BodyMutation      // body phases only
	continueRequest   *extprocv3.CommonResponse    // continue response for headers/body phases
	immediateResponse *extprocv3.ImmediateResponse // local reply (short-circuit)
	clearRouteCache   bool
}

// HeaderValue is the write-side carrier for a header value:
// set either Value or RawValue, never both.
type HeaderValue struct {
	Value    string
	RawValue []byte
}

func (hv HeaderValue) IsValid() bool {
	if len(hv.Value) > 0 && hv.RawValue != nil {
		return false
	}
	return true
}

// ToEnvoyHeaderValue converts to an Envoy header value (always via the
// RawValue channel to avoid encoding ambiguity).
func (hv HeaderValue) ToEnvoyHeaderValue(name string) *corev3.HeaderValue {
	if len(hv.Value) > 0 && hv.RawValue == nil {
		return &corev3.HeaderValue{
			Key:      name,
			RawValue: []byte(hv.Value),
		}
	}
	return &corev3.HeaderValue{
		Key:      name,
		RawValue: hv.RawValue,
	}
}

// ToEnvoyHeaderValueOption converts to a header action item; action must be a
// corev3.HeaderValueOption_HeaderAppendAction enum name.
func (hv HeaderValue) ToEnvoyHeaderValueOption(name string, action string) (*corev3.HeaderValueOption, error) {
	if err := validateMutableHeaderName(name); err != nil {
		return nil, err
	}
	if !hv.IsValid() {
		return nil, fmt.Errorf("invalid header value for %s", name)
	}
	if err := ValidateHeaderValue(headerValueBytes(hv)); err != nil {
		return nil, fmt.Errorf("invalid value for header %s: %w", name, err)
	}
	v, ok := corev3.HeaderValueOption_HeaderAppendAction_value[action]
	if !ok {
		return nil, fmt.Errorf("unknown header append action %q", action)
	}
	return &corev3.HeaderValueOption{
		Header:       hv.ToEnvoyHeaderValue(name),
		AppendAction: corev3.HeaderValueOption_HeaderAppendAction(v),
	}, nil
}

// headerValueBytes returns the bytes that will be written for hv.
func headerValueBytes(hv HeaderValue) []byte {
	if hv.RawValue != nil {
		return hv.RawValue
	}
	return []byte(hv.Value)
}

// BuildHeaderValuesFromMap is a convenience constructor: map -> HeaderValues.
func BuildHeaderValuesFromMap(headers map[string]string) map[string]HeaderValue {
	h := make(map[string]HeaderValue)
	for n := range headers {
		h[n] = HeaderValue{RawValue: []byte(headers[n])}
	}
	return h
}

// RequestContext holds the entire state of one stream (= one HTTP request):
// parsed pseudo headers, accumulated processing time, cross-phase shared data
// and the mutations to be returned. Its lifetime matches the stream and it is
// only accessed by that stream's single goroutine — lock-free by design.
type RequestContext struct {
	// Pseudo headers parsed in the request-headers phase. The body phases
	// cannot see headers, so anything needed later must be stashed via SetValue.
	Scheme    string
	Authority string
	Method    string
	Path      string
	FullPath  string
	RequestID string
	TraceID   string
	SpanID    string

	AllHeaders AllHeaders

	Started     time.Time
	Duration    time.Duration
	EndOfStream bool
	data        map[string]any
	ctx         context.Context

	phase    int
	response PhaseResponse
}

// data must be allocated here, not in initReqCtx: that only runs in the
// request headers phase, which request_header_mode: SKIP never reaches.
func newRequestContext() *RequestContext {
	return &RequestContext{
		data: make(map[string]any), // cross-phase shared data
		ctx:  context.Background(),
		response: PhaseResponse{
			headerMutation: &extprocv3.HeaderMutation{},
		},
	}
}

// NewRequestContext builds an empty request context. It is primarily useful
// for tests and for SDK users that drive the context directly; the production
// stream loop allocates it internally.
func NewRequestContext() *RequestContext { return newRequestContext() }

// Context returns the stream context. It is canceled when Envoy cancels the
// stream (including its message timeout), so plugin callouts should use it.
func (rc *RequestContext) Context() context.Context {
	if rc == nil || rc.ctx == nil {
		return context.Background()
	}
	return rc.ctx
}

// setContext is called once by the stream loop before any phase runs.
func (rc *RequestContext) setContext(ctx context.Context) {
	if ctx != nil {
		rc.ctx = ctx
	}
}

// initReqCtx initializes the context in the request-headers phase:
// parses pseudo headers and the full header map.
func initReqCtx(rc *RequestContext, headers *corev3.HeaderMap) error {
	rc.Started = time.Now()
	rc.Duration = 0

	eitherValue := func(h *corev3.HeaderValue) string {
		if h == nil {
			return ""
		}
		val := h.Value
		if len(h.RawValue) > 0 {
			val = string(h.RawValue)
		}
		return val
	}

	// Reset the response state for this phase.
	rc.ResetPhase()

	var err error
	rc.AllHeaders, err = genHeaders(headers)
	if err != nil {
		return fmt.Errorf("parsing headers failed: %w", err)
	}

	for _, h := range headers.Headers {
		switch h.Key {
		case ":scheme":
			rc.Scheme = eitherValue(h)

		case ":authority":
			rc.Authority = eitherValue(h)

		case ":method":
			rc.Method = eitherValue(h)

		case ":path":
			rc.FullPath = eitherValue(h)
			rc.Path = strings.Split(rc.FullPath, "?")[0]

		case "x-request-id":
			rc.RequestID = eitherValue(h)

		case "traceparent":
			if traceID, spanID, ok := parseTraceparent(eitherValue(h)); ok {
				rc.TraceID = traceID
				rc.SpanID = spanID
			}

		default:
		}
	}

	return nil
}

// GetValue reads cross-phase shared state; errors when absent.
func (rc *RequestContext) GetValue(name string) (any, error) {
	val, exists := rc.data[name]
	if exists {
		return val, nil
	}
	return nil, errors.New(name + " does not exist")
}

// SetValue writes cross-phase shared state.
func (rc *RequestContext) SetValue(name string, val any) error {
	if rc.data == nil {
		rc.data = make(map[string]any)
	}
	rc.data[name] = val
	return nil
}

// ResetPhase clears the response state of the current phase (called between
// stream messages): mutations never leak across phases, matching Envoy's
// per-phase application model.
func (rc *RequestContext) ResetPhase() error {
	rc.phase = REQUEST_PHASE_UNDETERMINED
	rc.EndOfStream = false
	rc.response.headerMutation = &extprocv3.HeaderMutation{}
	rc.response.bodyMutation = nil
	rc.response.continueRequest = nil
	rc.response.immediateResponse = nil
	rc.response.clearRouteCache = false
	return nil
}

func (rc *RequestContext) setPhase(phase int) {
	rc.phase = defaultPhaseIfInvalid(phase)
}

// GetPhase returns the current processing phase.
func (rc *RequestContext) GetPhase() int {
	return rc.phase
}

// GetPhaseName returns the readable name of the current phase.
func (rc *RequestContext) GetPhaseName() string {
	return RequestPhaseToString(rc.phase)
}

// ContinueRequest continues the request carrying all accumulated mutations
// (clears any immediate response). GetResponse unconditionally rebuilds the
// CommonResponse, so mutations added before or after this call are both kept.
func (rc *RequestContext) ContinueRequest() error {
	if rc.response.immediateResponse != nil {
		rc.response.immediateResponse = nil
	}

	rc.response.continueRequest = &extprocv3.CommonResponse{
		HeaderMutation:  rc.ensureHeaderMutation(),
		BodyMutation:    rc.response.bodyMutation,
		ClearRouteCache: rc.response.clearRouteCache,
	}

	return nil
}

// CancelRequest short-circuits: terminates the request with a local reply.
// headers are merged into headerMutation and attached to
// ImmediateResponse.Headers. Note: the protocol has no immediate response in
// the trailers phases.
func (rc *RequestContext) CancelRequest(status int32, headers map[string]HeaderValue, body string) error {
	if err := ValidateStatus(status); err != nil {
		return err
	}
	if err := rc.AppendHeaders(headers); err != nil {
		return err
	}
	rc.response.continueRequest = nil
	rc.response.immediateResponse = &extprocv3.ImmediateResponse{
		Status: &typev3.HttpStatus{
			Code: typev3.StatusCode(status),
		},
		Headers: rc.ensureHeaderMutation(),
		Body:    []byte(body),
	}
	return nil
}

// ClearRouteCache asks Envoy to clear the route cache when a mutation affects
// routing (request direction only).
func (rc *RequestContext) ClearRouteCache() {
	rc.response.clearRouteCache = true
}

func (rc *RequestContext) UnClearRouteCache() {
	rc.response.clearRouteCache = false
}

// GetResponse builds the final response for the current phase:
// with an immediateResponse set it short-circuits (except in trailers phases);
// otherwise the continue response is rebuilt unconditionally, folding in
// mutations added after the processor returned (e.g. self-timing headers) and
// defaulting "nothing set" to a plain continue.
func (rc *RequestContext) GetResponse(phase int) (*extprocv3.ProcessingResponse, error) {

	// Handle immediate responses first.
	if rc.response.immediateResponse != nil {
		switch phase {
		case REQUEST_PHASE_REQUEST_HEADERS, REQUEST_PHASE_REQUEST_BODY, REQUEST_PHASE_RESPONSE_HEADERS, REQUEST_PHASE_RESPONSE_BODY:
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ImmediateResponse{
					ImmediateResponse: rc.response.immediateResponse,
				},
			}, nil

		// Trailers phases have no immediate-response option (headers only).
		default:
		}
	}

	// Common response: rebuilt unconditionally (see doc comment).
	if err := rc.ContinueRequest(); err != nil {
		return nil, err
	}

	switch phase {
	case REQUEST_PHASE_REQUEST_HEADERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{
					Response: rc.response.continueRequest,
				},
			},
		}, nil

	case REQUEST_PHASE_REQUEST_BODY:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{
					Response: rc.response.continueRequest,
				},
			},
		}, nil

	case REQUEST_PHASE_REQUEST_TRAILERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestTrailers{
				RequestTrailers: &extprocv3.TrailersResponse{
					HeaderMutation: rc.ensureHeaderMutation(),
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_HEADERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{
					Response: rc.response.continueRequest,
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_BODY:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{
					Response: rc.response.continueRequest,
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_TRAILERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseTrailers{
				ResponseTrailers: &extprocv3.TrailersResponse{
					HeaderMutation: rc.ensureHeaderMutation(),
				},
			},
		}, nil

	default:
		return nil, errors.New("unknown request phase")
	}
}

// EmptyContinueResponse is equivalent to GetResponse but carries no mutations
// at all — used as the fallback when a processor set nothing.
func (rc *RequestContext) EmptyContinueResponse(phase int) (*extprocv3.ProcessingResponse, error) {
	switch phase {
	case REQUEST_PHASE_REQUEST_HEADERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{
					Response: &extprocv3.CommonResponse{},
				},
			},
		}, nil

	case REQUEST_PHASE_REQUEST_BODY:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{
					Response: &extprocv3.CommonResponse{},
				},
			},
		}, nil

	case REQUEST_PHASE_REQUEST_TRAILERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestTrailers{
				RequestTrailers: &extprocv3.TrailersResponse{
					HeaderMutation: &extprocv3.HeaderMutation{},
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_HEADERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{
					Response: &extprocv3.CommonResponse{},
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_BODY:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{
					Response: &extprocv3.CommonResponse{},
				},
			},
		}, nil

	case REQUEST_PHASE_RESPONSE_TRAILERS:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseTrailers{
				ResponseTrailers: &extprocv3.TrailersResponse{
					HeaderMutation: &extprocv3.HeaderMutation{},
				},
			},
		}, nil

	default:
		return nil, errors.New("unknown request phase")
	}
}

// PhaseSnapshot captures the mutation and request-scoped state of the current
// phase. It is used by the responsibility chain to roll back a plugin that
// failed under the fail_open policy, so that its partial header/body changes
// and shared values never reach Envoy or later plugins. The fields are
// intentionally unexported: callers treat it as opaque state.
type PhaseSnapshot struct {
	headerMutation    *extprocv3.HeaderMutation
	bodyMutation      *extprocv3.BodyMutation
	continueRequest   *extprocv3.CommonResponse
	immediateResponse *extprocv3.ImmediateResponse
	clearRouteCache   bool
	data              map[string]any
}

// SnapshotPhase captures the current phase mutations for later rollback.
func (rc *RequestContext) SnapshotPhase() PhaseSnapshot {
	return PhaseSnapshot{
		headerMutation:    cloneHeaderMutation(rc.response.headerMutation),
		bodyMutation:      rc.response.bodyMutation,
		continueRequest:   rc.response.continueRequest,
		immediateResponse: rc.response.immediateResponse,
		clearRouteCache:   rc.response.clearRouteCache,
		data:              cloneData(rc.data),
	}
}

// RestorePhase rolls the current phase mutations back to a snapshot.
func (rc *RequestContext) RestorePhase(snapshot PhaseSnapshot) {
	rc.response.headerMutation = cloneHeaderMutation(snapshot.headerMutation)
	rc.response.bodyMutation = snapshot.bodyMutation
	rc.response.continueRequest = snapshot.continueRequest
	rc.response.immediateResponse = snapshot.immediateResponse
	rc.response.clearRouteCache = snapshot.clearRouteCache
	rc.data = cloneData(snapshot.data)
}

func cloneData(in map[string]any) map[string]any {
	if in == nil {
		return make(map[string]any)
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneHeaderMutation copies the header mutation slices. The option values are
// immutable once appended (plugins only append through the RequestContext
// API), so a shallow copy of the slice is sufficient.
func cloneHeaderMutation(in *extprocv3.HeaderMutation) *extprocv3.HeaderMutation {
	if in == nil {
		return &extprocv3.HeaderMutation{}
	}
	return &extprocv3.HeaderMutation{
		SetHeaders:    slices.Clone(in.SetHeaders),
		RemoveHeaders: slices.Clone(in.RemoveHeaders),
	}
}

// ensureHeaderMutation returns a non-nil header mutation accumulator.
func (rc *RequestContext) ensureHeaderMutation() *extprocv3.HeaderMutation {
	if rc.response.headerMutation == nil {
		rc.response.headerMutation = &extprocv3.HeaderMutation{}
	}
	return rc.response.headerMutation
}

// UpdateHeader appends one header action with an explicit write action.
func (rc *RequestContext) UpdateHeader(name string, hv HeaderValue, action string) error {
	if err := validateMutableHeaderName(name); err != nil {
		return err
	}
	hm := rc.ensureHeaderMutation()
	ho, err := hv.ToEnvoyHeaderValueOption(name, action)
	if err != nil {
		return err
	}
	hm.SetHeaders = append(hm.SetHeaders, ho)
	return nil
}

// AppendHeader appends: keeps existing values and adds a new one.
func (rc *RequestContext) AppendHeader(name string, hv HeaderValue) error {
	return rc.UpdateHeader(name, hv, "APPEND_IF_EXISTS_OR_ADD")
}

// AddHeader adds only when the header is absent.
func (rc *RequestContext) AddHeader(name string, hv HeaderValue) error {
	return rc.UpdateHeader(name, hv, "ADD_IF_ABSENT")
}

// OverwriteHeader replaces any existing value.
func (rc *RequestContext) OverwriteHeader(name string, hv HeaderValue) error {
	return rc.UpdateHeader(name, hv, "OVERWRITE_IF_EXISTS_OR_ADD")
}

// UpdateHeaders batch-writes headers with an explicit action.
func (rc *RequestContext) UpdateHeaders(headers map[string]HeaderValue, action string) error {
	hos := make([]*corev3.HeaderValueOption, 0, len(headers))
	for n, hv := range headers {
		if err := validateMutableHeaderName(n); err != nil {
			return err
		}
		ho, err := hv.ToEnvoyHeaderValueOption(n, action)
		if err != nil {
			return err
		}
		hos = append(hos, ho)
	}
	hm := rc.ensureHeaderMutation()
	hm.SetHeaders = append(hm.SetHeaders, hos...)
	return nil
}

func (rc *RequestContext) AppendHeaders(headers map[string]HeaderValue) error {
	return rc.UpdateHeaders(headers, "APPEND_IF_EXISTS_OR_ADD")
}

func (rc *RequestContext) AddHeaders(headers map[string]HeaderValue) error {
	return rc.UpdateHeaders(headers, "ADD_IF_ABSENT")
}

func (rc *RequestContext) OverwriteHeaders(headers map[string]HeaderValue) error {
	return rc.UpdateHeaders(headers, "OVERWRITE_IF_EXISTS_OR_ADD")
}

// RemoveHeader removes a header (deduplicated).
func (rc *RequestContext) RemoveHeader(name string) error {
	if err := validateMutableHeaderName(name); err != nil {
		return err
	}
	hm := rc.ensureHeaderMutation()
	if !slices.Contains(hm.RemoveHeaders, name) {
		hm.RemoveHeaders = append(hm.RemoveHeaders, name)
	}
	return nil
}

func (rc *RequestContext) RemoveHeaders(headers []string) error {
	for _, h := range headers {
		if err := validateMutableHeaderName(h); err != nil {
			return err
		}
	}
	hm := rc.ensureHeaderMutation()
	for _, h := range headers {
		if !slices.Contains(hm.RemoveHeaders, h) {
			hm.RemoveHeaders = append(hm.RemoveHeaders, h)
		}
	}
	return nil
}

func (rc *RequestContext) RemoveHeadersVariadic(headers ...string) error {
	for _, h := range headers {
		if err := validateMutableHeaderName(h); err != nil {
			return err
		}
	}
	hm := rc.ensureHeaderMutation()
	for _, h := range headers {
		if !slices.Contains(hm.RemoveHeaders, h) {
			hm.RemoveHeaders = append(hm.RemoveHeaders, h)
		}
	}
	return nil
}

// ReplaceBodyChunk replaces the body bytes of the current message.
//
// It deliberately leaves Content-Length alone: under a STREAMED body mode the
// chunk is only part of the body, so its length is not the message length. Use
// ReplaceBody when the filter is configured BUFFERED and this call carries the
// whole body.
//
// An empty body is a no-op; use ClearBodyChunk or ClearBody to remove a body.
func (rc *RequestContext) ReplaceBodyChunk(body []byte) error {
	if len(body) == 0 {
		return nil
	}

	rc.response.bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{
			Body: body,
		},
	}

	return nil
}

// ReplaceBody replaces the whole body.
//
// Only correct when the filter is configured BUFFERED, so that a single body
// message carries the entire body. Under STREAMED use ReplaceBodyChunk. The
// Content-Length header is deliberately left alone: Envoy recalculates it when
// a filter mutates the body, and a single streamed chunk is not the full body.
func (rc *RequestContext) ReplaceBody(body []byte) error {
	return rc.ReplaceBodyChunk(body)
}

// ClearBodyChunk removes the body bytes of the current message, leaving
// Content-Length alone. See ReplaceBodyChunk on why.
func (rc *RequestContext) ClearBodyChunk() error {
	rc.response.bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_ClearBody{
			ClearBody: true,
		},
	}
	return nil
}

// ClearBody removes the body. Content-Length is left to Envoy, matching
// ReplaceBody.
func (rc *RequestContext) ClearBody() error {
	return rc.ClearBodyChunk()
}
