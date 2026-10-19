package processor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

var (
	registerOnce sync.Once
	callLog      []string
	seenContext  context.Context
	closeCounter atomic.Int64
)

type testPlugin struct {
	plugin.BasePlugin
	name       string
	header     string
	fail       bool
	clearRoute bool
	cancel     bool
	closeCount *atomic.Int64
}

func (p *testPlugin) Name() string { return p.name }

func (p *testPlugin) OnRequestHeaders(ctx plugin.Context, _ extproc.AllHeaders) error {
	callLog = append(callLog, p.name+"-headers")
	if seenContext == nil {
		seenContext = ctx.Context()
	}
	if p.clearRoute {
		ctx.ClearRouteCache()
	}
	if p.header != "" {
		if err := ctx.OverwriteHeader(p.header, extproc.HeaderValue{Value: "1"}); err != nil {
			return err
		}
	}
	if p.cancel {
		if err := ctx.CancelRequest(403, nil, "denied"); err != nil {
			return err
		}
	}
	if p.fail {
		return errors.New("boom")
	}
	return nil
}

func (p *testPlugin) OnRequestBody(_ plugin.Context, _ []byte) error {
	callLog = append(callLog, p.name+"-body")
	return nil
}

func (p *testPlugin) Close() error {
	if p.closeCount != nil {
		p.closeCount.Add(1)
	}
	return nil
}

func registerTestPlugins() {
	registerOnce.Do(func() {
		plugin.Register("pt-bad", func() plugin.Plugin {
			return &testPlugin{name: "pt-bad", header: "x-bad", fail: true}
		})
		plugin.Register("pt-after", func() plugin.Plugin {
			return &testPlugin{name: "pt-after", header: "x-after"}
		})
		plugin.Register("pt-route", func() plugin.Plugin {
			return &testPlugin{name: "pt-route", header: "x-route", clearRoute: true}
		})
		plugin.Register("pt-cancel", func() plugin.Plugin {
			return &testPlugin{name: "pt-cancel", cancel: true}
		})
		plugin.Register("pt-close", func() plugin.Plugin {
			return &testPlugin{name: "pt-close", closeCount: &closeCounter}
		})
	})
}

func boolPtr(v bool) *bool { return &v }

func newProcessor(t *testing.T, failurePolicy string, plugins ...plugin.PluginConfig) (*ChainProcessor, *plugin.RouteTable) {
	t.Helper()
	registerTestPlugins()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	table, err := plugin.BuildRouteTable(plugin.ChainConfig{FailurePolicy: failurePolicy, Plugins: plugins}, nil, logger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	proc := New("test", logger)
	proc.SwapChain(table)
	return proc, table
}

func TestProcessorRollsBackFailOpenPlugin(t *testing.T) {
	callLog = nil
	seenContext = nil

	proc, _ := newProcessor(t, "fail_open",
		plugin.PluginConfig{Name: "pt-bad", Enabled: boolPtr(true), Order: 10},
		plugin.PluginConfig{Name: "pt-after", Enabled: boolPtr(true), Order: 20},
	)
	rc := extproc.NewRequestContext()
	rc.Authority = "api.example.com"
	rc.Path = "/v1/chat/completions"
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err != nil {
		t.Fatalf("fail_open must absorb the plugin error: %v", err)
	}
	if len(callLog) != 2 || callLog[0] != "pt-bad-headers" || callLog[1] != "pt-after-headers" {
		t.Fatalf("unexpected plugin execution: %v", callLog)
	}
	if seenContext == nil {
		t.Fatal("plugin.Context must expose a non-nil stream context")
	}

	resp, err := rc.GetResponse(extproc.REQUEST_PHASE_REQUEST_HEADERS)
	if err != nil {
		t.Fatal(err)
	}
	headers := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	if len(headers) != 1 || headers[0].GetHeader().GetKey() != "x-after" {
		t.Fatalf("the failed plugin's header mutation was not rolled back: %v", headers)
	}
}

func TestProcessorClearRouteCache(t *testing.T) {
	callLog = nil
	proc, _ := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-route", Enabled: boolPtr(true), Order: 10},
	)
	rc := extproc.NewRequestContext()
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err != nil {
		t.Fatalf("ProcessRequestHeaders failed: %v", err)
	}
	resp, err := rc.GetResponse(extproc.REQUEST_PHASE_REQUEST_HEADERS)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetRequestHeaders().GetResponse().GetClearRouteCache() {
		t.Fatal("ClearRouteCache from a plugin must reach the Envoy response")
	}
}

func TestProcessorShortCircuitStopsLaterPlugins(t *testing.T) {
	callLog = nil
	proc, _ := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-cancel", Enabled: boolPtr(true), Order: 10},
		plugin.PluginConfig{Name: "pt-after", Enabled: boolPtr(true), Order: 20},
	)
	rc := extproc.NewRequestContext()
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err != nil {
		t.Fatalf("a short-circuit is not a chain error: %v", err)
	}
	if len(callLog) != 1 {
		t.Fatalf("plugins after the short-circuit must not run: %v", callLog)
	}
	resp, err := rc.GetResponse(extproc.REQUEST_PHASE_REQUEST_HEADERS)
	if err != nil {
		t.Fatal(err)
	}
	immediate := resp.GetImmediateResponse()
	if immediate == nil || immediate.GetStatus().GetCode() != 403 {
		t.Fatalf("expected a 403 immediate response, got %v", immediate)
	}
}

func TestProcessorFallsBackToGlobalChainForBodyOnlyStreams(t *testing.T) {
	callLog = nil
	proc, _ := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-after", Enabled: boolPtr(true), Order: 10},
	)
	rc := extproc.NewRequestContext()
	// No request-headers phase ran, so the chain is resolved from the global
	// table instead of per-stream state.
	if err := proc.ProcessRequestBody(rc, []byte("{}")); err != nil {
		t.Fatalf("ProcessRequestBody failed: %v", err)
	}
	if len(callLog) != 1 || callLog[0] != "pt-after-body" {
		t.Fatalf("global chain fallback failed: %v", callLog)
	}
}

func TestProcessorDisabledPluginIsNotBuilt(t *testing.T) {
	callLog = nil
	proc, table := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-after", Enabled: boolPtr(false), Order: 10},
	)
	if got := table.Names(); len(got) != 0 {
		t.Fatalf("disabled plugin must not join the table: %v", got)
	}
	rc := extproc.NewRequestContext()
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err != nil {
		t.Fatalf("empty chain failed: %v", err)
	}
	if len(callLog) != 0 {
		t.Fatalf("disabled plugin must not run: %v", callLog)
	}
}

func TestProcessorWithoutRouteTableFailsCleanly(t *testing.T) {
	t.Parallel()

	proc := New("test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	rc := extproc.NewRequestContext()
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err == nil {
		t.Fatal("a processor without a route table must return an error, not panic")
	}
	if err := proc.ProcessResponseHeaders(rc, extproc.AllHeaders{}); err == nil {
		t.Fatal("a processor without a route table must return an error, not panic")
	}
}

func TestOldGenerationClosesAfterLastStream(t *testing.T) {
	closeCounter.Store(0)
	proc, _ := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-close", Enabled: boolPtr(true), Order: 10},
	)
	rc := extproc.NewRequestContext()
	rc2 := extproc.NewRequestContext()
	proc.StreamStart(rc)
	proc.StreamStart(rc2)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	next, err := plugin.BuildRouteTable(plugin.ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins:       []plugin.PluginConfig{{Name: "pt-close", Enabled: boolPtr(true), Order: 10}},
	}, nil, logger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	proc.SwapChain(next)
	if got := closeCounter.Load(); got != 0 {
		t.Fatalf("a retired generation with an in-flight stream must not be closed: %d", got)
	}

	proc.StreamEnd(rc)
	if got := closeCounter.Load(); got != 0 {
		t.Fatalf("the retired generation must stay open while another stream is active: %d", got)
	}
	proc.StreamEnd(rc2)
	if got := closeCounter.Load(); got != 1 {
		t.Fatalf("the retired generation must be closed after its last stream: %d", got)
	}

	proc.Close(0)
	if got := closeCounter.Load(); got != 2 {
		t.Fatalf("the live generation must be closed on process shutdown: %d", got)
	}
	// Close must be idempotent.
	proc.Close(0)
	if got := closeCounter.Load(); got != 2 {
		t.Fatalf("Close must be idempotent: %d", got)
	}
}

func TestOldGenerationClosesImmediatelyWithoutStreams(t *testing.T) {
	closeCounter.Store(0)
	proc, _ := newProcessor(t, "fail_closed",
		plugin.PluginConfig{Name: "pt-close", Enabled: boolPtr(true), Order: 10},
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	next, err := plugin.BuildRouteTable(plugin.ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins:       []plugin.PluginConfig{{Name: "pt-close", Enabled: boolPtr(true), Order: 10}},
	}, nil, logger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	proc.SwapChain(next)
	if got := closeCounter.Load(); got != 1 {
		t.Fatalf("a generation with no streams must close immediately on swap: %d", got)
	}
	proc.Close(0)
	if got := closeCounter.Load(); got != 2 {
		t.Fatalf("the live generation must be closed on shutdown: %d", got)
	}
}

func TestMetricsObserverRecordsPluginOutcomes(t *testing.T) {
	registerTestPlugins()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := extproc.NewEmptyMetrics()
	registry := prometheus.NewRegistry()
	metrics.Register(registry)

	observer := &MetricsObserver{}
	observer.SetMetrics(metrics)
	table, err := plugin.BuildRouteTable(plugin.ChainConfig{
		FailurePolicy: "fail_open",
		Plugins: []plugin.PluginConfig{
			{Name: "pt-bad", Enabled: boolPtr(true), Order: 10},
			{Name: "pt-after", Enabled: boolPtr(true), Order: 20},
		},
	}, nil, logger, observer)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	proc := New("test", logger)
	proc.SwapChain(table)
	rc := extproc.NewRequestContext()
	proc.StreamStart(rc)
	defer proc.StreamEnd(rc)
	if err := proc.ProcessRequestHeaders(rc, extproc.AllHeaders{}); err != nil {
		t.Fatalf("ProcessRequestHeaders failed: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var failures, durations int
	for _, family := range families {
		switch family.GetName() {
		case "extproc_plugin_failures_total":
			for _, metric := range family.GetMetric() {
				labels := map[string]string{}
				for _, label := range metric.GetLabel() {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["plugin"] == "pt-bad" && labels["phase"] == "request_headers" && labels["policy"] == "fail_open" {
					failures++
				}
			}
		case "extproc_plugin_duration_seconds":
			durations = len(family.GetMetric())
		}
	}
	if failures != 1 {
		t.Fatalf("expected one fail_open plugin failure metric, got %d", failures)
	}
	if durations != 2 {
		t.Fatalf("expected duration metrics for both plugins, got %d", durations)
	}
}

func TestStreamContextRequestInfo(t *testing.T) {
	t.Parallel()

	rc := extproc.NewRequestContext()
	rc.Scheme = "https"
	rc.Authority = "api.example.com"
	rc.Method = "POST"
	rc.Path = "/v1/chat"
	rc.FullPath = "/v1/chat?x=1"
	rc.RequestID = "req-1"
	rc.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	rc.SpanID = "00f067aa0ba902b7"

	info := newStreamContext(rc).RequestInfo()
	if info.Scheme != rc.Scheme || info.Authority != rc.Authority || info.Method != rc.Method ||
		info.Path != rc.Path || info.FullPath != rc.FullPath || info.RequestID != rc.RequestID ||
		info.TraceID != rc.TraceID || info.SpanID != rc.SpanID {
		t.Fatalf("request info mismatch: %+v", info)
	}
}
