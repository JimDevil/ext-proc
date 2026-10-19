package plugin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func boolPtr(v bool) *bool { return &v }

// registerTestPlugin registers a test plugin and removes it afterwards so the
// package tests are repeatable with -count=2.
func registerTestPlugin(t *testing.T, name string, factory Factory) {
	t.Helper()
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	})
	Register(name, factory)
}

// fakeContext is an in-memory Context for this package's tests (it avoids
// importing plugintest, which would create an import cycle).
type fakeContext struct {
	values  map[string]any
	aborted bool
	status  int32

	checkpoint    map[string]any
	checkpointSet bool
	cpAborted     bool
	cpStatus      int32
}

func newFakeContext() *fakeContext { return &fakeContext{values: map[string]any{}} }

func (f *fakeContext) ContinueRequest() error { return nil }

func (f *fakeContext) CancelRequest(status int32, _ map[string]extproc.HeaderValue, _ string) error {
	f.aborted = true
	f.status = status
	return nil
}

func (f *fakeContext) AppendHeader(string, extproc.HeaderValue) error    { return nil }
func (f *fakeContext) OverwriteHeader(string, extproc.HeaderValue) error { return nil }
func (f *fakeContext) RemoveHeader(string) error                         { return nil }
func (f *fakeContext) ReplaceBody([]byte) error                          { return nil }
func (f *fakeContext) EndOfStream() bool                                 { return true }
func (f *fakeContext) SetValue(name string, val any) error {
	f.values[name] = val
	return nil
}
func (f *fakeContext) GetValue(name string) (any, bool) {
	v, ok := f.values[name]
	return v, ok
}
func (f *fakeContext) Aborted() bool { return f.aborted }

func (f *fakeContext) Context() context.Context { return context.Background() }

func (f *fakeContext) RequestInfo() RequestInfo { return RequestInfo{} }

func (f *fakeContext) ClearRouteCache() {}

func (f *fakeContext) BeginPlugin() {
	f.checkpoint = make(map[string]any, len(f.values))
	for k, v := range f.values {
		f.checkpoint[k] = v
	}
	f.cpAborted = f.aborted
	f.cpStatus = f.status
	f.checkpointSet = true
}

func (f *fakeContext) RollbackPlugin() {
	if !f.checkpointSet {
		return
	}
	f.values = make(map[string]any, len(f.checkpoint))
	for k, v := range f.checkpoint {
		f.values[k] = v
	}
	f.aborted = f.cpAborted
	f.status = f.cpStatus
	f.checkpointSet = false
}

var _ Context = (*fakeContext)(nil)

// stubPlugin records the call order for chain tests.
type stubPlugin struct {
	BasePlugin
	name string
	seq  *[]string
	// onRequestHeaders replaces the default behavior when set (injects
	// errors / short-circuits).
	onRequestHeaders func(ctx Context, headers extproc.AllHeaders) error
}

func (s *stubPlugin) Name() string { return s.name }

func (s *stubPlugin) OnRequestHeaders(ctx Context, headers extproc.AllHeaders) error {
	*s.seq = append(*s.seq, s.name)
	if s.onRequestHeaders != nil {
		return s.onRequestHeaders(ctx, headers)
	}
	return nil
}

func newChain(failOpen bool, plugins ...Plugin) *Chain {
	entries := make([]pluginEntry, 0, len(plugins))
	for _, p := range plugins {
		entries = append(entries, pluginEntry{plugin: p, failOpen: failOpen})
	}
	return &Chain{entries: entries, logger: testLogger}
}

func TestChainRunsPluginsInOrder(t *testing.T) {
	t.Parallel()

	var seq []string
	c := newChain(false,
		&stubPlugin{name: "a", seq: &seq},
		&stubPlugin{name: "b", seq: &seq},
		&stubPlugin{name: "c", seq: &seq},
	)

	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err != nil {
		t.Fatalf("the chain should run without errors: %v", err)
	}
	want := []string{"a", "b", "c"}
	for i, name := range want {
		if seq[i] != name {
			t.Fatalf("plugin execution order mismatch: got %v, want %v", seq, want)
		}
	}
}

func TestChainStopsOnShortCircuit(t *testing.T) {
	t.Parallel()

	var seq []string
	cancelPlugin := &stubPlugin{name: "b", seq: &seq, onRequestHeaders: func(ctx Context, _ extproc.AllHeaders) error {
		return ctx.CancelRequest(401, nil, "denied")
	}}

	c := newChain(false,
		&stubPlugin{name: "a", seq: &seq},
		cancelPlugin,
		&stubPlugin{name: "c", seq: &seq},
	)

	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err != nil {
		t.Fatalf("a short-circuit is not a chain-level error: %v", err)
	}
	if !ctx.Aborted() {
		t.Fatal("ctx should be in the aborted state")
	}
	if !ctx.aborted || ctx.status != 401 {
		t.Fatalf("short-circuit status mismatch: %d", ctx.status)
	}
	if len(seq) != 2 {
		t.Fatalf("plugins after the short-circuit must not run: %v", seq)
	}
}

func TestChainFailOpenSkipsFailedPlugin(t *testing.T) {
	t.Parallel()

	var seq []string
	boom := &stubPlugin{name: "boom", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
		return errors.New("boom")
	}}

	c := newChain(true, // fail_open
		boom,
		&stubPlugin{name: "after", seq: &seq},
	)

	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err != nil {
		t.Fatalf("fail_open must not escalate the error: %v", err)
	}
	if len(seq) != 2 || seq[1] != "after" {
		t.Fatalf("fail_open: the following plugins should still run: %v", seq)
	}
}

func TestChainFailClosedReturnsError(t *testing.T) {
	t.Parallel()

	var seq []string
	boom := &stubPlugin{name: "boom", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
		return errors.New("boom")
	}}

	c := newChain(false, // fail_closed
		boom,
		&stubPlugin{name: "after", seq: &seq},
	)

	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err == nil {
		t.Fatal("fail_closed should escalate the error")
	}
	if len(seq) != 1 {
		t.Fatalf("fail_closed: the following plugins must not run: %v", seq)
	}
}

// TestPluginFailurePolicyOverride verifies that a plugin-level policy wins
// over the chain default: under fail_closed, an explicitly fail_open plugin is
// skipped while an uncovered failure still escalates.
func TestPluginFailurePolicyOverride(t *testing.T) {
	t.Parallel()

	var seq []string
	registerTestPlugin(t, "fp-open", func() Plugin {
		return &stubPlugin{name: "fp-open", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
			return errors.New("fp-open boom")
		}}
	})
	registerTestPlugin(t, "fp-closed", func() Plugin {
		return &stubPlugin{name: "fp-closed", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
			return errors.New("fp-closed boom")
		}}
	})
	registerTestPlugin(t, "fp-ok", func() Plugin { return &stubPlugin{name: "fp-ok", seq: &seq} })

	compose := func(cfg ChainConfig) *Chain {
		t.Helper()
		table, err := BuildRouteTable(cfg, nil, testLogger)
		if err != nil {
			t.Fatalf("BuildRouteTable failed: %v", err)
		}
		return table.Compose("any.test", "/")
	}

	cfg := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []PluginConfig{
			{Name: "fp-open", Enabled: boolPtr(true), Order: 10, FailurePolicy: "fail_open"},
			{Name: "fp-ok", Enabled: boolPtr(true), Order: 20},
		},
	}
	if err := compose(cfg).RunRequestHeaders(newFakeContext(), extproc.AllHeaders{}); err != nil {
		t.Fatalf("a plugin-level fail_open must absorb the error: %v", err)
	}
	if len(seq) != 2 {
		t.Fatalf("the plugin after a fail_open plugin must run: %v", seq)
	}

	cfg2 := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []PluginConfig{
			{Name: "fp-closed", Enabled: boolPtr(true), Order: 10},
			{Name: "fp-ok", Enabled: boolPtr(true), Order: 20},
		},
	}
	if err := compose(cfg2).RunRequestHeaders(newFakeContext(), extproc.AllHeaders{}); err == nil {
		t.Fatal("an uncovered error must follow the chain-level fail_closed policy")
	}
}

// TestInvalidFailurePolicy verifies that invalid failure policies fail fast at
// build time, including the chain-level value.
func TestInvalidFailurePolicy(t *testing.T) {
	t.Parallel()

	registerTestPlugin(t, "fp-bogus", func() Plugin { return &stubPlugin{name: "fp-bogus", seq: &[]string{}} })

	cfg := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins:       []PluginConfig{{Name: "fp-bogus", Enabled: boolPtr(true), Order: 10, FailurePolicy: "bogus"}},
	}
	if _, err := BuildRouteTable(cfg, nil, testLogger); err == nil {
		t.Fatal("an invalid plugin-level failurePolicy must fail the build")
	}

	cfgChain := ChainConfig{
		FailurePolicy: "bogus",
		Plugins:       []PluginConfig{{Name: "fp-bogus", Enabled: boolPtr(true), Order: 10}},
	}
	if _, err := BuildRouteTable(cfgChain, nil, testLogger); err == nil {
		t.Fatal("an invalid chain-level failurePolicy must fail the build")
	}
}

// TestBuildRouteTableInitFailure verifies that a plugin Init error propagates.
func TestBuildRouteTableInitFailure(t *testing.T) {
	t.Parallel()

	registerTestPlugin(t, "build-init-err", func() Plugin { return &initErrPlugin{} })
	cfg := ChainConfig{Plugins: []PluginConfig{{Name: "build-init-err", Enabled: boolPtr(true)}}}
	if _, err := BuildRouteTable(cfg, nil, testLogger); err == nil {
		t.Fatal("a plugin Init failure must propagate")
	}
}

// initErrPlugin is a stub whose Init always fails.
type initErrPlugin struct {
	BasePlugin
}

func (p *initErrPlugin) Name() string { return "build-init-err" }

func (p *initErrPlugin) Init(*gjson.Json, *slog.Logger) error {
	return errors.New("init failed")
}

func TestChainFailOpenRollsBackPanic(t *testing.T) {
	t.Parallel()

	var seq []string
	boom := &stubPlugin{name: "boom", seq: &seq, onRequestHeaders: func(ctx Context, _ extproc.AllHeaders) error {
		if err := ctx.SetValue("leak", "yes"); err != nil {
			return err
		}
		if err := ctx.CancelRequest(403, nil, "partial"); err != nil {
			return err
		}
		panic("boom")
	}}
	after := &stubPlugin{name: "after", seq: &seq, onRequestHeaders: func(ctx Context, _ extproc.AllHeaders) error {
		if _, ok := ctx.GetValue("leak"); ok {
			t.Error("fail_open must roll back the failed plugin's state")
		}
		if ctx.Aborted() {
			t.Error("fail_open must roll back the failed plugin's short-circuit")
		}
		return nil
	}}

	c := newChain(true, boom, after)
	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err != nil {
		t.Fatalf("fail_open must absorb the panic: %v", err)
	}
	if len(seq) != 2 {
		t.Fatalf("the chain must continue after a recovered panic: %v", seq)
	}
	if ctx.Aborted() {
		t.Fatal("the rolled-back short-circuit must not survive")
	}
}

func TestChainFailClosedRecoversPanicAsError(t *testing.T) {
	t.Parallel()

	var seq []string
	boom := &stubPlugin{name: "boom", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
		panic("boom")
	}}
	c := newChain(false, boom, &stubPlugin{name: "after", seq: &seq})
	err := c.RunRequestHeaders(newFakeContext(), extproc.AllHeaders{})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("fail_closed must surface the panic as a regular plugin error: %v", err)
	}
	if len(seq) != 1 {
		t.Fatalf("plugins after the failed one must not run: %v", seq)
	}
}

func TestChainFailOpenRollsBackPartialMutations(t *testing.T) {
	t.Parallel()

	var seq []string
	partial := &stubPlugin{name: "partial", seq: &seq, onRequestHeaders: func(ctx Context, _ extproc.AllHeaders) error {
		if err := ctx.SetValue("partial", "yes"); err != nil {
			return err
		}
		return errors.New("boom")
	}}
	after := &stubPlugin{name: "after", seq: &seq, onRequestHeaders: func(ctx Context, _ extproc.AllHeaders) error {
		if _, ok := ctx.GetValue("partial"); ok {
			t.Error("fail_open must roll back partial mutations")
		}
		return nil
	}}
	c := newChain(true, partial, after)
	if err := c.RunRequestHeaders(newFakeContext(), extproc.AllHeaders{}); err != nil {
		t.Fatalf("fail_open must absorb the error: %v", err)
	}
	if len(seq) != 2 {
		t.Fatalf("the chain must continue after a rolled-back plugin: %v", seq)
	}
}
