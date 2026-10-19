package extproc

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

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
