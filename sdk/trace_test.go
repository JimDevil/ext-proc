package extproc

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

func TestParseTraceparent(t *testing.T) {
	t.Parallel()

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	trace, span, ok := parseTraceparent("00-" + traceID + "-" + spanID + "-01")
	if !ok || trace != traceID || span != spanID {
		t.Fatalf("valid traceparent rejected: (%q,%q,%v)", trace, span, ok)
	}

	invalid := []string{
		"",
		"00-" + traceID + "-" + spanID,
		"00-" + traceID + "-" + spanID + "-01-extra-is-ok",
		"ff-" + traceID + "-" + spanID + "-01",
		"00-00000000000000000000000000000000-" + spanID + "-01",
		"00-" + traceID + "-0000000000000000-01",
		"00-" + traceID + "-" + spanID + "-zz",
		"00-" + traceID + "-" + spanID + "-0",
		"00-" + traceID + "-" + spanID + "-01-",
		"00-" + traceID + "-" + spanID + "-01-extra",
	}
	for _, value := range invalid {
		if trace, span, ok := parseTraceparent(value); ok {
			t.Fatalf("invalid traceparent %q accepted: (%q,%q)", value, trace, span)
		}
	}
}

func TestRequestHeadersCaptureTraceContext(t *testing.T) {
	t.Parallel()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const spanID = "00f067aa0ba902b7"
	srv := &GenericExtProcServer{
		name:      "noop",
		processor: noopProcessor{},
		options:   NewDefaultProcessingOptions(),
		metrics:   NewEmptyMetrics(),
		logger:    testSdkLogger,
	}
	rc := NewRequestContext()
	req := &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
				{Key: ":path", Value: "/v1/chat"},
				{Key: "traceparent", Value: "00-" + traceID + "-" + spanID + "-01"},
			}},
		},
	}}
	if _, _, err := srv.processPhase(req, srv.processor, rc); err != nil {
		t.Fatalf("processPhase failed: %v", err)
	}
	if rc.TraceID != traceID || rc.SpanID != spanID {
		t.Fatalf("trace context mismatch: trace=%q span=%q", rc.TraceID, rc.SpanID)
	}
}
