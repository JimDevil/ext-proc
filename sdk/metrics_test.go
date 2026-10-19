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
// per-phase duration histogram records a sample labeled by phase.
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

func TestStreamDurationHasClassicBuckets(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics := NewEmptyMetrics().Register(registry)
	metrics.StreamDurationSeconds.Observe(0.25)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "extproc_stream_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			hist := metric.GetHistogram()
			if hist == nil {
				t.Fatal("stream duration is not a histogram")
			}
			if len(hist.GetBucket()) < 2 {
				t.Fatalf("expected classic buckets in addition to +Inf, got %d", len(hist.GetBucket()))
			}
			return
		}
	}
	t.Fatal("extproc_stream_duration_seconds not found")
}

func TestPluginMetricsLabels(t *testing.T) {
	t.Parallel()

	registry := prometheus.NewRegistry()
	metrics := NewEmptyMetrics().Register(registry)
	metrics.PluginDuration.WithLabelValues("model-router", "request_body").Observe(0.003)
	metrics.PluginFailures.WithLabelValues("apikey-extract", "request_headers", "fail_open").Inc()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	foundDuration, foundFailure := false, false
	for _, family := range families {
		switch family.GetName() {
		case "extproc_plugin_duration_seconds":
			foundDuration = true
			labels := family.GetMetric()[0].GetLabel()
			if len(labels) != 2 {
				t.Fatalf("plugin duration must have plugin/phase labels: %v", labels)
			}
		case "extproc_plugin_failures_total":
			foundFailure = true
			labels := family.GetMetric()[0].GetLabel()
			if len(labels) != 3 {
				t.Fatalf("plugin failures must have plugin/phase/policy labels: %v", labels)
			}
		}
	}
	if !foundDuration || !foundFailure {
		t.Fatalf("plugin metrics missing: duration=%v failures=%v", foundDuration, foundFailure)
	}
}
