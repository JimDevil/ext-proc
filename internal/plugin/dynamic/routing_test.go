package dynamic

import (
	"testing"

	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/plugin/plugintest"
	"github.com/gogf/gf/v2/encoding/gjson"
)

// TestRouteTableWithDynamicPlugin drives BuildRouteTable end to end with a
// url-carrying plugin: the loader spawns the fixture binary, per-route
// variants get their own subprocess+config, and CloseAll reclaims them.
func TestRouteTableWithDynamicPlugin(t *testing.T) {
	plugin.SetDynamicLoader(New(t.TempDir()))

	tree := gjson.New(map[string]any{
		"chain": map[string]any{
			"plugins": []any{
				map[string]any{
					"name": "echo-test", "enabled": true, "order": 10,
					"url":    "file://" + echoBin,
					"config": map[string]any{"value": "global"},
					"matchRules": []any{
						map[string]any{"domain": []string{"*.test"}, "config": map[string]any{"value": "variant"}},
					},
				},
			},
		},
	})
	cfg := plugin.ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []plugin.PluginConfig{{
			Name: "echo-test", Enabled: true, Order: 10,
			URL:        "file://" + echoBin,
			MatchRules: []plugin.MatchRule{{Domain: []string{"*.test"}}},
		}},
	}

	table, err := plugin.BuildRouteTable(cfg, tree, testLog)
	if err != nil {
		t.Fatalf("BuildRouteTable: %v", err)
	}
	defer table.CloseAll()

	// Matching domain → the rule variant's subprocess (config value=variant).
	chain := table.Compose("a.test", "/x")
	mctx := plugintest.NewMockContext()
	if err := chain.RunRequestHeaders(mctx, allHeaders(":path", "/x")); err != nil {
		t.Fatalf("RunRequestHeaders (variant): %v", err)
	}
	if v, ok := mctx.LastHeaderValue("x-echo"); !ok || v != "variant" {
		t.Fatalf("variant config did not reach the subprocess: %v %v", v, ok)
	}

	// Non-matching domain → the global instance (config value=global).
	chain = table.Compose("other.corp", "/x")
	gctx := plugintest.NewMockContext()
	if err := chain.RunRequestHeaders(gctx, allHeaders(":path", "/x")); err != nil {
		t.Fatalf("RunRequestHeaders (global): %v", err)
	}
	if v, ok := gctx.LastHeaderValue("x-echo"); !ok || v != "global" {
		t.Fatalf("global config did not reach the subprocess: %v %v", v, ok)
	}
}

// TestRouteTableDynamicCloseAllReclaims processes: after CloseAll the fixture
// subprocesses are gone (a follow-up phase call on the closed adapter fails).
func TestRouteTableDynamicCloseAllReclaims(t *testing.T) {
	plugin.SetDynamicLoader(New(t.TempDir()))

	tree := gjson.New(map[string]any{
		"chain": map[string]any{
			"plugins": []any{
				map[string]any{"name": "echo-test", "enabled": true, "order": 10, "url": "file://" + echoBin},
			},
		},
	})
	cfg := plugin.ChainConfig{
		Plugins: []plugin.PluginConfig{{Name: "echo-test", Enabled: true, Order: 10, URL: "file://" + echoBin}},
	}
	table, err := plugin.BuildRouteTable(cfg, tree, testLog)
	if err != nil {
		t.Fatalf("BuildRouteTable: %v", err)
	}
	chain := table.Compose("any.test", "/x")
	if err := chain.RunRequestHeaders(plugintest.NewMockContext(), allHeaders(":path", "/x")); err != nil {
		t.Fatalf("RunRequestHeaders: %v", err)
	}

	table.CloseAll()

	// The adapter is closed; a further phase call must report the closed
	// process instead of hanging.
	again := plugintest.NewMockContext()
	if err := chain.RunRequestHeaders(again, allHeaders(":path", "/x")); err == nil {
		t.Fatal("expected an error after CloseAll, got none")
	}
}
