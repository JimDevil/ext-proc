package plugin

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeContext is an in-memory Context for this package's tests (it avoids
// importing plugintest, which would create an import cycle).
type fakeContext struct {
	values  map[string]any
	aborted bool
	status  int32
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

// TestPluginFailurePolicyOverride 验证插件级 failurePolicy 覆盖链级默认：
// 链级 fail_closed 下，显式声明 fail_open 的插件报错时被跳过（请求继续），
// 未覆盖的插件报错仍上抛（fail_closed 生效）。
func TestPluginFailurePolicyOverride(t *testing.T) {
	t.Parallel()

	var seq []string
	Register("fp-open", func() Plugin {
		return &stubPlugin{name: "fp-open", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
			return errors.New("fp-open boom")
		}}
	})
	Register("fp-closed", func() Plugin {
		return &stubPlugin{name: "fp-closed", seq: &seq, onRequestHeaders: func(Context, extproc.AllHeaders) error {
			return errors.New("fp-closed boom")
		}}
	})
	Register("fp-ok", func() Plugin { return &stubPlugin{name: "fp-ok", seq: &seq} })

	// 场景1：链 fail_closed，fp-open 显式 fail_open → 跳过继续，fp-ok 正常执行
	cfg := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []PluginConfig{
			{Name: "fp-open", Enabled: true, Order: 10, FailurePolicy: "fail_open"},
			{Name: "fp-ok", Enabled: true, Order: 20},
		},
	}
	c, err := BuildChain(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("BuildChain failed: %v", err)
	}
	ctx := newFakeContext()
	if err := c.RunRequestHeaders(ctx, extproc.AllHeaders{}); err != nil {
		t.Fatalf("插件级 fail_open 应消化错误: %v", err)
	}
	if len(seq) != 2 {
		t.Fatalf("插件级 fail_open 后续插件应执行: %v", seq)
	}

	// 场景2：链 fail_closed，fp-closed 未覆盖 → 错误上抛
	cfg2 := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []PluginConfig{
			{Name: "fp-closed", Enabled: true, Order: 10},
			{Name: "fp-ok", Enabled: true, Order: 20},
		},
	}
	c2, err := BuildChain(cfg2, nil, testLogger)
	if err != nil {
		t.Fatalf("BuildChain failed: %v", err)
	}
	ctx2 := newFakeContext()
	if err := c2.RunRequestHeaders(ctx2, extproc.AllHeaders{}); err == nil {
		t.Fatal("未覆盖的插件错误应按链级 fail_closed 上抛")
	}
}

// TestInvalidFailurePolicy 验证非法的 failurePolicy 值在构建期 fail fast。
func TestInvalidFailurePolicy(t *testing.T) {
	t.Parallel()

	Register("fp-bogus", func() Plugin { return &stubPlugin{name: "fp-bogus", seq: &[]string{}} })

	// 插件级非法值
	cfg := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins:       []PluginConfig{{Name: "fp-bogus", Enabled: true, Order: 10, FailurePolicy: "bogus"}},
	}
	if _, err := BuildChain(cfg, nil, testLogger); err == nil {
		t.Fatal("插件级非法 failurePolicy 应报错")
	}

	// 链级非法值（插件未覆盖时暴露）
	cfgChain := ChainConfig{
		FailurePolicy: "bogus",
		Plugins:       []PluginConfig{{Name: "fp-bogus", Enabled: true, Order: 10}},
	}
	if _, err := BuildChain(cfgChain, nil, testLogger); err == nil {
		t.Fatal("链级非法 failurePolicy 应报错")
	}
}

func TestBuildChain(t *testing.T) {
	t.Parallel()

	Register("build-a", func() Plugin { return &stubPlugin{name: "build-a", seq: &[]string{}} })
	Register("build-b", func() Plugin { return &stubPlugin{name: "build-b", seq: &[]string{}} })
	Register("build-init-err", func() Plugin { return &initErrPlugin{} })

	cfg := ChainConfig{
		FailurePolicy: "fail_open",
		Plugins: []PluginConfig{
			{Name: "build-b", Enabled: true, Order: 10},
			{Name: "build-disabled", Enabled: false, Order: 5},
			{Name: "build-a", Enabled: true, Order: 20},
		},
	}

	c, err := BuildChain(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("failed to build the chain: %v", err)
	}
	// Disabled plugins are filtered; order decides position (b=10 before a=20).
	if names := c.Names(); len(names) != 2 || names[0] != "build-b" || names[1] != "build-a" {
		t.Fatalf("chain orchestration mismatch: %v", names)
	}

	// An unregistered plugin errors out.
	_, err = BuildChain(ChainConfig{Plugins: []PluginConfig{{Name: "no-such", Enabled: true}}}, nil, testLogger)
	if err == nil {
		t.Fatal("an unregistered plugin should error")
	}

	// A plugin Init failure propagates.
	_, err = BuildChain(ChainConfig{Plugins: []PluginConfig{{Name: "build-init-err", Enabled: true}}}, nil, testLogger)
	if err == nil {
		t.Fatal("a plugin Init failure should propagate")
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

func TestPluginConfigNode(t *testing.T) {
	t.Parallel()

	tree := gjson.New(map[string]any{
		"chain": map[string]any{
			"plugins": []any{
				map[string]any{"name": "p1", "enabled": true, "order": 10, "config": map[string]any{"k": "v1"}},
				map[string]any{"name": "p2", "enabled": true, "order": 20},
			},
		},
	})

	if node := pluginConfigNode(tree, 0); node == nil || node.Get("k").String() != "v1" {
		t.Fatalf("expected p1's config node: %v", node)
	}
	if node := pluginConfigNode(tree, 1); node != nil {
		t.Fatalf("a plugin without a config section should yield nil: %v", node)
	}
	if node := pluginConfigNode(tree, 99); node != nil {
		t.Fatalf("an out-of-range index should yield nil: %v", node)
	}
	if node := pluginConfigNode(nil, 0); node != nil {
		t.Fatalf("an empty config tree should yield nil: %v", node)
	}
	if node := pluginConfigNodeByName(tree, "p1"); node == nil || node.Get("k").String() != "v1" {
		t.Fatalf("lookup by name should find p1's config node: %v", node)
	}
}
