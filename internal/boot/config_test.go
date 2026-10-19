package boot

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/JimDevil/ext-proc/internal/model/config"
	"github.com/JimDevil/ext-proc/internal/plugin"

	_ "github.com/JimDevil/ext-proc/internal/plugins/apikeyextract"
	_ "github.com/JimDevil/ext-proc/internal/plugins/modelrouter"
)

// TestInitConfigLoadsSampleConfig is the guard against config typos: the
// repository's sample config must decode, validate and build a real route
// table with the sample plugins.
func TestInitConfigLoadsSampleConfig(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	t.Setenv("EXT_PROC_CONFIG", filepath.Join(root, "manifest", "config", "config.yaml"))

	if err := InitConfig(context.Background()); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}
	snap := config.Current()
	if snap.Raw == nil {
		t.Fatal("raw config tree is nil")
	}
	if snap.Cfg.App.Name != "ext-proc" {
		t.Fatalf("app.name mismatch: %q", snap.Cfg.App.Name)
	}
	if snap.Cfg.Logger.Level != "info" {
		t.Fatalf("logger.level mismatch: %q", snap.Cfg.Logger.Level)
	}
	if snap.Cfg.Server.BindAddress != "0.0.0.0" {
		t.Fatalf("bindAddress mismatch: %q", snap.Cfg.Server.BindAddress)
	}
	if snap.Cfg.Server.MaxRecvMsgSize != 16<<20 {
		t.Fatalf("maxRecvMsgSize mismatch: %d", snap.Cfg.Server.MaxRecvMsgSize)
	}
	if len(snap.Cfg.Chain.Plugins) != 2 {
		t.Fatalf("expected two sample plugins, got %d", len(snap.Cfg.Chain.Plugins))
	}
	for i, pc := range snap.Cfg.Chain.Plugins {
		if !pc.IsEnabled() {
			t.Fatalf("sample plugin %d (%s) should be enabled", i, pc.Name)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	table, err := plugin.BuildRouteTable(snap.Cfg.Chain, snap.Raw, logger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	if got := table.Names(); len(got) != 2 || got[0] != "model-router" || got[1] != "apikey-extract" {
		t.Fatalf("unexpected chain composition: %v", got)
	}
	if got := table.Compose("api.example.com", "/v1/chat/completions").Names(); len(got) != 2 {
		t.Fatalf("unexpected composed chain: %v", got)
	}
}

// TestReloadIsTransactional verifies that a failed rebuild keeps the previous
// snapshot/chain, and that a successful rebuild publishes the new one.
func TestReloadIsTransactional(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	t.Setenv("EXT_PROC_CONFIG", filepath.Join(root, "manifest", "config", "config.yaml"))
	if err := InitConfig(context.Background()); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}
	config.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := InitChain(context.Background()); err != nil {
		t.Fatalf("InitChain failed: %v", err)
	}

	old := config.Current()
	bad := *old
	bad.Cfg.Chain.Plugins = append(append([]plugin.PluginConfig(nil), old.Cfg.Chain.Plugins...), plugin.PluginConfig{
		Name:    "does-not-exist",
		Order:   99,
		Enabled: boolPtr(true),
	})
	if err := config.ApplyReload(context.Background(), &bad); err == nil {
		t.Fatal("a reload with an unknown plugin must fail")
	}
	if got := config.Current(); got != old {
		t.Fatal("a failed reload must keep the previous snapshot")
	}

	good := *old
	good.Cfg.Chain.Plugins = append([]plugin.PluginConfig(nil), old.Cfg.Chain.Plugins...)
	good.Cfg.Chain.Plugins[0].Order = 5
	if err := config.ApplyReload(context.Background(), &good); err != nil {
		t.Fatalf("valid reload failed: %v", err)
	}
	if got := config.Current(); got == old {
		t.Fatal("a successful reload must publish a new snapshot")
	}
	if got := config.Current().Cfg.Chain.Plugins[0].Order; got != 5 {
		t.Fatalf("new config was not published: order=%d", got)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestReloadPicksUpPluginPrivateConfig guards the raw-tree path used by plugin
// Init: changing only a plugin-private config key must produce a new
// generation, even though the typed Chain struct itself is unchanged.
func TestReloadPicksUpPluginPrivateConfig(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	src := filepath.Join(root, "manifest", "config", "config.yaml")
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	t.Setenv("EXT_PROC_CONFIG", path)

	ctx := context.Background()
	if err := InitConfig(ctx); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}
	config.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := InitChain(ctx); err != nil {
		t.Fatalf("InitChain failed: %v", err)
	}
	if err := InitWatch(ctx); err != nil {
		t.Fatalf("InitWatch failed: %v", err)
	}
	// Stop the fs watcher so this test drives exactly one reload through
	// cfgLoader.Load; the reload path itself is what is under test.
	if _, err := cfgLoader.StopWatch(ctx); err != nil {
		t.Fatalf("StopWatch failed: %v", err)
	}

	modified := bytes.ReplaceAll(original,
		[]byte("modelToHeader: x-higress-llm-model"),
		[]byte("modelToHeader: x-reloaded-model"))
	if bytes.Equal(modified, original) {
		t.Fatal("test fixture did not contain the expected modelToHeader value")
	}
	if err := os.WriteFile(path, modified, 0o600); err != nil {
		t.Fatalf("write modified config: %v", err)
	}
	// The watcher is stopped in this test, so drop the adapter's parsed-file
	// cache manually to emulate the invalidation the watcher normally does.
	cfgAdapter.Clear()
	if err := cfgLoader.Load(ctx); err != nil {
		t.Fatalf("reload failed: %v", err)
	}

	got := config.Current().Raw.Get("chain.plugins.0.config.modelToHeader").String()
	if got != "x-reloaded-model" {
		t.Fatalf("plugin-private config was not reloaded: got %q", got)
	}

	// A duplicate watcher event for the same file must be coalesced instead of
	// rebuilding an identical generation.
	before := config.Current()
	if err := cfgLoader.Load(ctx); err != nil {
		t.Fatalf("duplicate reload failed: %v", err)
	}
	if config.Current() != before {
		t.Fatal("a duplicate reload event must not publish a new generation")
	}
}
