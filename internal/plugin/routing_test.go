package plugin

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/gogf/gf/v2/encoding/gjson"
)

// capturePlugin records the raw config JSON it was Init'd with, so tests can
// assert on the merged config each variant received.
type capturePlugin struct {
	BasePlugin
	name string
	got  *[]string
}

func (p *capturePlugin) Name() string { return p.name }

func (p *capturePlugin) Init(cfg *gjson.Json, _ *slog.Logger) error {
	if cfg == nil {
		*p.got = append(*p.got, "<nil>")
		return nil
	}
	b, err := json.Marshal(cfg.Map())
	if err != nil {
		return err
	}
	*p.got = append(*p.got, string(b))
	return nil
}

func TestRouteTableCompose(t *testing.T) {
	t.Parallel()

	// Slots (in chain order):
	//   cap:  global + one rule (chat.example.com + /v1/chat → config override)
	//   sw:   global + one rule (*.internal.corp → disable)
	//   open: global only (no rules)
	var capGot, swGot, openGot []string
	Register("rt-cap", func() Plugin { return &capturePlugin{name: "rt-cap", got: &capGot} })
	Register("rt-sw", func() Plugin { return &capturePlugin{name: "rt-sw", got: &swGot} })
	Register("rt-open", func() Plugin { return &capturePlugin{name: "rt-open", got: &openGot} })

	tree := gjson.New(map[string]any{
		"chain": map[string]any{
			"plugins": []any{
				map[string]any{
					"name": "rt-cap", "enabled": true, "order": 10,
					"config":     map[string]any{"a": "global", "keep": "g"},
					"matchRules": []any{map[string]any{"domain": []string{"chat.example.com"}, "pathPrefix": "/v1/chat", "config": map[string]any{"a": "rule"}}},
				},
				map[string]any{
					"name": "rt-sw", "enabled": true, "order": 20,
					"matchRules": []any{map[string]any{"domain": []string{"*.internal.corp"}, "disable": true}},
				},
				map[string]any{"name": "rt-open", "enabled": true, "order": 30},
			},
		},
	})

	cfg := ChainConfig{
		FailurePolicy: "fail_closed",
		Plugins: []PluginConfig{
			{Name: "rt-cap", Enabled: true, Order: 10, MatchRules: []MatchRule{{Domain: []string{"chat.example.com"}, PathPrefix: "/v1/chat"}}},
			{Name: "rt-sw", Enabled: true, Order: 20, MatchRules: []MatchRule{{Domain: []string{"*.internal.corp"}, Disable: true}}},
			{Name: "rt-open", Enabled: true, Order: 30},
		},
	}

	table, err := BuildRouteTable(cfg, tree, testLogger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	if table.RuleCount() != 2 {
		t.Fatalf("total rule count mismatch: %d", table.RuleCount())
	}

	tests := []struct {
		authority string
		path      string
		want      []string // expected composition (plugin names in chain order)
	}{
		// The rule-scoped override only affects rt-cap; the others stay global.
		{"chat.example.com", "/v1/chat/completions", []string{"rt-cap", "rt-sw", "rt-open"}},
		// Port stripped; case-insensitive.
		{"chat.example.com:8443", "/v1/chat", []string{"rt-cap", "rt-sw", "rt-open"}},
		// rt-sw disabled on internal domains; rt-cap falls back to global.
		{"x.internal.corp", "/anything", []string{"rt-cap", "rt-open"}},
		// Nothing matches: pure global composition.
		{"other.com", "/", []string{"rt-cap", "rt-sw", "rt-open"}},
	}
	for _, tt := range tests {
		got := table.Compose(tt.authority, tt.path)
		if names := got.Names(); !equalNames(names, tt.want) {
			t.Fatalf("Compose(%q, %q) composition mismatch: got %v, want %v", tt.authority, tt.path, names, tt.want)
		}
	}

	// Per-plugin independence: on chat.example.com rt-cap used the rule config
	// (a=rule, keep=g survived the deep merge) while rt-open used its global
	// (nil — no config section).
	if len(capGot) < 2 {
		t.Fatalf("rt-cap should be initialized per variant: %v", capGot)
	}
	if !strings.Contains(capGot[0], `"a":"global"`) || !strings.Contains(capGot[0], `"keep":"g"`) {
		t.Fatalf("rt-cap global config mismatch: %s", capGot[0])
	}
	if !strings.Contains(capGot[1], `"a":"rule"`) || !strings.Contains(capGot[1], `"keep":"g"`) {
		t.Fatalf("rt-cap variant config merge mismatch: %s", capGot[1])
	}
	if len(openGot) != 1 || openGot[0] != "<nil>" {
		t.Fatalf("rt-open (no matchRules) should be built exactly once from a nil config: %v", openGot)
	}
	// rt-sw: the disable marker builds no instance — only the global one runs
	// Init.
	if len(swGot) != 1 {
		t.Fatalf("disable rules must not build a variant instance: %v", swGot)
	}
}

func TestRouteTableFallbackIsGlobal(t *testing.T) {
	t.Parallel()

	var got []string
	Register("rt-fallback", func() Plugin { return &capturePlugin{name: "rt-fallback", got: &got} })

	cfg := ChainConfig{
		Plugins: []PluginConfig{{Name: "rt-fallback", Enabled: true, Order: 10}},
	}
	table, err := BuildRouteTable(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	// nil tree: the plugin is built from a nil config node and every request
	// composes the same single-plugin chain.
	chain := table.Compose("anything.test", "/x")
	if names := chain.Names(); len(names) != 1 || names[0] != "rt-fallback" {
		t.Fatalf("composition mismatch: %v", names)
	}
	if got[0] != "<nil>" {
		t.Fatalf("expected a nil config node: %v", got)
	}
}

func TestBuildRouteTableUnknownPlugin(t *testing.T) {
	t.Parallel()

	// A plugin that is not registered anywhere must fail the build.
	cfg := ChainConfig{
		Plugins: []PluginConfig{{Name: "no-such-plugin", Enabled: true, Order: 10}},
	}
	_, err := BuildRouteTable(cfg, nil, testLogger)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("an unregistered plugin should fail fast: %v", err)
	}
}

func TestRouteTableSortsByOrder(t *testing.T) {
	t.Parallel()

	// Config lists the plugins out of order; the table must still compose the
	// chain in ascending order (config-file order on ties).
	var sortGot []string
	Register("rt-sort-late", func() Plugin { return &capturePlugin{name: "rt-sort-late", got: &sortGot} })
	Register("rt-sort-early", func() Plugin { return &capturePlugin{name: "rt-sort-early", got: &sortGot} })

	cfg := ChainConfig{
		Plugins: []PluginConfig{
			{Name: "rt-sort-late", Enabled: true, Order: 30},
			{Name: "rt-sort-early", Enabled: true, Order: 10},
		},
	}
	table, err := BuildRouteTable(cfg, nil, testLogger)
	if err != nil {
		t.Fatalf("BuildRouteTable failed: %v", err)
	}
	want := []string{"rt-sort-early", "rt-sort-late"}
	if names := table.Names(); !equalNames(names, want) {
		t.Fatalf("slot order mismatch: got %v, want %v", names, want)
	}
	if names := table.Compose("any.test", "/x").Names(); !equalNames(names, want) {
		t.Fatalf("composed chain order mismatch: got %v, want %v", names, want)
	}
}

func TestDomainMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host    string
		domains []string
		want    bool
	}{
		{"example.com", []string{"example.com"}, true},
		{"example.com", []string{"*.example.com"}, false}, // wildcard does not match the bare domain
		{"a.example.com", []string{"*.example.com"}, true},
		{"a.b.example.com", []string{"*.example.com"}, true},
		{"notexample.com", []string{"*.example.com"}, false}, // suffix must be dot-anchored
		{"anything.org", []string{"*"}, true},
		{"Example.COM", []string{"example.com"}, true}, // case-insensitive
	}
	for _, tt := range tests {
		if got := domainMatches(tt.host, tt.domains); got != tt.want {
			t.Fatalf("domainMatches(%q, %v) = %v, want %v", tt.host, tt.domains, got, tt.want)
		}
	}
}

func TestDeepMergeMaps(t *testing.T) {
	t.Parallel()

	dst := map[string]any{"a": "global", "nested": map[string]any{"x": 1, "y": 2}}
	src := map[string]any{"a": "rule", "nested": map[string]any{"y": 3, "z": 4}}

	got := deepMergeMaps(dst, src)

	// Scalars: src wins. Maps: recursive merge. New keys are added.
	if got["a"] != "rule" {
		t.Fatalf("scalar override mismatch: %v", got["a"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["x"] != 1 || nested["y"] != 3 || nested["z"] != 4 {
		t.Fatalf("nested merge mismatch: %v", got["nested"])
	}

	// Inputs must not be mutated.
	if dst["a"] != "global" || dst["nested"].(map[string]any)["y"] != 2 {
		t.Fatalf("inputs were mutated: %v", dst)
	}
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
