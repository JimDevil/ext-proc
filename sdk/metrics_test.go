package extproc

import (
	"context"
	"net"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// quietProcessor does nothing — the phase still runs through the dispatch
// path, which is what the duration histogram measures.
type quietProcessor struct {
	opts *ProcessingOptions
}

func (p *quietProcessor) GetName() string                                           { return "quiet-test" }
func (p *quietProcessor) GetOptions() *ProcessingOptions                            { return p.opts }
func (p *quietProcessor) ProcessRequestHeaders(*RequestContext, AllHeaders) error   { return nil }
func (p *quietProcessor) ProcessRequestBody(*RequestContext, []byte) error          { return nil }
func (p *quietProcessor) ProcessRequestTrailers(*RequestContext, AllHeaders) error  { return nil }
func (p *quietProcessor) ProcessResponseHeaders(*RequestContext, AllHeaders) error  { return nil }
func (p *quietProcessor) ProcessResponseBody(*RequestContext, []byte) error         { return nil }
func (p *quietProcessor) ProcessResponseTrailers(*RequestContext, AllHeaders) error { return nil }
func (p *quietProcessor) ErrorHandler(*RequestContext, int, error)                  {}
func (p *quietProcessor) Close(int32) error                                         { return nil }

// TestPhaseDurationMetric serves a phase over bufconn and asserts the
// per-phase duration histogram records a sample labeled by phase — the
// metrics groundwork for phase-level tracing.
func TestPhaseDurationMetric(t *testing.T) {
	metrics := NewEmptyMetrics()
	registry := prometheus.NewRegistry()
	metrics.Register(registry)

	opts := NewDefaultProcessingOptions()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	srv := &GenericExtProcServer{
		name:      "phase-duration-test",
		processor: &quietProcessor{opts: opts},
		options:   opts,
		metrics:   metrics,
		logger:    testSdkLogger,
	}
	epb.RegisterExternalProcessorServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Drive the request-headers phase (one sample expected).
	stream, err := epb.NewExternalProcessorClient(conn).Process(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(&epb.ProcessingRequest{Request: &epb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &epb.HttpHeaders{Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
			{Key: ":path", RawValue: []byte("/chat/completions")},
			{Key: "x-request-id", RawValue: []byte("trace-1")},
		}}},
	}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	samples := 0
	for _, mf := range families {
		if mf.GetName() != "extproc_phase_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "phase" && l.GetValue() == "REQUEST_HEADERS" {
					samples += int(m.GetHistogram().GetSampleCount())
				}
			}
		}
	}
	if samples == 0 {
		t.Fatal("extproc_phase_duration_seconds recorded no sample for REQUEST_HEADERS")
	}
}
