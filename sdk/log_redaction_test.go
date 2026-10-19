package extproc

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

type noopProcessor struct{}

func (noopProcessor) GetName() string                                          { return "noop" }
func (noopProcessor) GetOptions() *ProcessingOptions                           { return NewDefaultProcessingOptions() }
func (noopProcessor) ProcessRequestHeaders(*RequestContext, AllHeaders) error  { return nil }
func (noopProcessor) ProcessResponseHeaders(*RequestContext, AllHeaders) error { return nil }
func (noopProcessor) ProcessRequestBody(*RequestContext, []byte) error         { return nil }
func (noopProcessor) ProcessResponseBody(*RequestContext, []byte) error        { return nil }
func (noopProcessor) ProcessRequestTrailers(*RequestContext, AllHeaders) error { return nil }
func (noopProcessor) ProcessResponseTrailers(*RequestContext, AllHeaders) error {
	return nil
}
func (noopProcessor) ErrorHandler(*RequestContext, int, error) {}
func (noopProcessor) Close(int32) error                        { return nil }

// TestDebugLogsDoNotLeakHeadersOrBodies guards the security contract that raw
// request/response data is never logged, even at debug level.
func TestDebugLogsDoNotLeakHeadersOrBodies(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := &GenericExtProcServer{
		name:      "noop",
		processor: noopProcessor{},
		options:   NewDefaultProcessingOptions(),
		metrics:   NewEmptyMetrics(),
		logger:    logger,
	}
	rc := NewRequestContext()

	headerReq := &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{
			Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
				{Key: ":method", Value: "POST"},
				{Key: ":authority", Value: "api.example.com"},
				{Key: ":path", Value: "/v1/chat/completions"},
				{Key: "x-request-id", Value: "req-123"},
				{Key: "traceparent", Value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
				{Key: "authorization", RawValue: []byte("Bearer super-secret-token")},
				{Key: "x-api-key", RawValue: []byte("super-secret-key")},
				{Key: "cookie", RawValue: []byte("session=secret-cookie")},
			}},
			EndOfStream: true,
		},
	}}
	if _, _, err := srv.processPhase(headerReq, srv.processor, rc); err != nil {
		t.Fatalf("headers phase failed: %v", err)
	}

	bodyReq := &epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestBody{
		RequestBody: &epb.HttpBody{Body: []byte(`{"prompt":"body-secret"}`), EndOfStream: true},
	}}
	if _, _, err := srv.processPhase(bodyReq, srv.processor, rc); err != nil {
		t.Fatalf("body phase failed: %v", err)
	}

	got := buf.String()
	for _, secret := range []string{"super-secret-token", "super-secret-key", "secret-cookie", "body-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("debug log leaked %q:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "req-123") {
		t.Fatalf("metadata-only logging should keep the request id:\n%s", got)
	}
	if !strings.Contains(got, "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("metadata-only logging should keep the trace id:\n%s", got)
	}
}
