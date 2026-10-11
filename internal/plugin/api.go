// Package plugin defines the plugin contract for this ext_proc server
// (filter-chain pattern).
//
// Every plugin implements the Plugin interface and self-registers in its
// package init(); at runtime a Chain invokes enabled plugins in the configured
// order at each processing phase, and the first plugin that short-circuits
// stops the chain. Plugins have zero coupling to each other, own their own
// config section, and support individual switches plus hot reload.
package plugin

import (
	"log/slog"
	"sync/atomic"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
)

// ChainConfig is the orchestration config for one chain (defined here, in the
// consuming package, and referenced by config.Config). Plugin-private configs
// are not part of the typed struct: BuildChain extracts each plugin's config
// node from the raw config tree by plugin name.
type ChainConfig struct {
	// FailurePolicy is the unified policy for plugin execution failures:
	// fail_closed (default, abort the request) / fail_open (skip and continue).
	FailurePolicy string         `json:"failurePolicy" yaml:"failurePolicy"`
	Plugins       []PluginConfig `json:"plugins"       yaml:"plugins"`
}

// MatchRule scopes ONE plugin to a set of routes (the higress wasmPlugin
// matchRules model, but owned per plugin). A rule matches when ALL of its
// dimensions match; rules are evaluated in order and the FIRST match wins;
// requests matching no rule use the plugin's global config.
type MatchRule struct {
	// Domain matches the request authority: exact name, "*.example.com" style
	// wildcards, or "*" for any. The port is stripped and the match is
	// case-insensitive.
	Domain []string `json:"domain" yaml:"domain"`
	// PathPrefix matches the request path by prefix; empty matches all paths.
	PathPrefix string `json:"pathPrefix" yaml:"pathPrefix"`
	// Disable turns THIS plugin off for the matched routes (other plugins are
	// unaffected). The rule's config override is ignored when disabled.
	Disable bool `json:"disable" yaml:"disable"`
}

// PluginConfig is the orchestration entry for a single plugin (one item of
// chain.plugins in config.yaml). The plugin-private `config` child node and
// its `matchRules` config overrides are intentionally not typed beyond the
// match dimensions: BuildRouteTable extracts the raw config nodes from the
// config tree and hands them to the plugin's own Init, keeping every plugin's
// configuration modular by construction.
type PluginConfig struct {
	// Name must match the plugin's registered name.
	Name string `json:"name" yaml:"name"`
	// Enabled is the individual switch; disabled plugins never join the chain.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Order defines chain position, ascending; equal orders keep the order
	// they appear in the config file.
	Order int `json:"order" yaml:"order"`
	// FailurePolicy optionally overrides chain.failurePolicy for THIS plugin:
	// fail_closed aborts the request when the plugin errors, fail_open logs
	// and skips it. Empty inherits the chain-level policy. MatchRule variants
	// inherit the resolved policy of their plugin entry.
	FailurePolicy string `json:"failurePolicy" yaml:"failurePolicy"`
	// MatchRules optionally scope THIS plugin to routes: the first matching
	// rule wins (its config overrides the plugin's global config, or Disable
	// switches the plugin off for those routes); requests matching no rule use
	// the global config. Each plugin owns its rules independently — one
	// plugin's rule has no effect on the others.
	MatchRules []MatchRule `json:"matchRules" yaml:"matchRules"`
	// URL optionally loads the plugin as a DYNAMIC SUBPROCESS PLUGIN instead
	// of a compiled-in package:
	//   oci://registry/repo:tag (or @sha256:digest) — the image is pulled at
	//     load time into the local plugin cache and its executable is spawned
	//     as a child process speaking the ext_proc protocol (anonymous public
	//     registry pulls; digest-pinned refs are recommended for production);
	//   file:///abs/path — a local plugin binary (development / e2e).
	// When set, the plugin need not be compiled into this binary and the
	// factory registry is not consulted. Every instance (global + matchRule
	// variants) runs its own subprocess with the instance's merged config.
	URL string `json:"url" yaml:"url"`
	// Timeout bounds ONE phase round trip to a dynamic plugin's subprocess
	// ("300ms", "2s", ...; default 5s). Ignored for compiled-in plugins.
	Timeout string `json:"timeout" yaml:"timeout"`
}

// Context is the narrow interface plugins use to operate on the current
// request. The production implementation bridges the SDK's RequestContext
// (internal/processor); the test implementation lives in
// internal/plugin/plugintest. Plugins depend on this interface only — never on
// SDK stream state directly.
type Context interface {
	// ContinueRequest lets the request through (clears any immediate response).
	// The SDK generates the continue response after the chain finishes, so
	// plugins rarely need to call this explicitly.
	ContinueRequest() error
	// CancelRequest short-circuits: terminates the request with a local reply;
	// the rest of this phase and all later phases are skipped.
	// Note: the protocol has no immediate response in the trailers phases.
	CancelRequest(status int32, headers map[string]extproc.HeaderValue, body string) error
	// AppendHeader appends a header (keeps existing values with the same name).
	AppendHeader(name string, hv extproc.HeaderValue) error
	// OverwriteHeader replaces a header.
	OverwriteHeader(name string, hv extproc.HeaderValue) error
	// RemoveHeader removes a header (:method/:authority/host/x-envoy-* are
	// protected by Envoy and will be rejected there).
	RemoveHeader(name string) error
	// ReplaceBody replaces the current body wholesale (requires Envoy's
	// request_body_mode to be BUFFERED).
	ReplaceBody(body []byte) error
	// EndOfStream reports whether the current body message is the last chunk
	// (always true under BUFFERED).
	EndOfStream() bool
	// SetValue writes request-scoped state shared across plugins and phases
	// (unique per stream).
	SetValue(name string, val any) error
	// GetValue reads request-scoped shared state; returns false when absent.
	GetValue(name string) (any, bool)
	// Aborted reports whether the request has been short-circuited (some
	// plugin called CancelRequest).
	Aborted() bool
}

// Plugin is the contract every plugin implements. Embed BasePlugin and only
// override the phases you care about.
type Plugin interface {
	// Name matches chain.plugins[].name in config.yaml.
	Name() string
	// Init is called once when the chain is built / hot-reloaded.
	// cfg is this plugin's config node (nil when the section is absent —
	// provide defaults; individual fields may also be missing, check via
	// Get(...).String() style accessors). Returning an error fails startup
	// (fail fast).
	Init(cfg *gjson.Json, logger *slog.Logger) error
	// Close releases plugin resources. It runs on graceful shutdown AND when
	// a hot-reload-retired chain's TTL expires (reclaiming dynamic subprocess
	// plugins). Plugins must tolerate Close being called at any point after
	// the instance left the live chain; still do NOT hold resources that
	// require explicit release in instances (put connections in package-level
	// singletons instead) — see README.
	Close() error

	// The six phase callbacks. header/trailer keys are lower cased (guaranteed
	// by the SDK); pseudo headers (:path/:method/:authority...) are only
	// visible in the corresponding headers phase.
	OnRequestHeaders(ctx Context, headers extproc.AllHeaders) error
	OnRequestBody(ctx Context, body []byte) error
	OnRequestTrailers(ctx Context, trailers extproc.AllHeaders) error
	OnResponseHeaders(ctx Context, headers extproc.AllHeaders) error
	OnResponseBody(ctx Context, body []byte) error
	OnResponseTrailers(ctx Context, trailers extproc.AllHeaders) error
}

// BasePlugin provides empty defaults for every phase and the lifecycle; embed
// it and override only what you need.
type BasePlugin struct{}

func (BasePlugin) Init(*gjson.Json, *slog.Logger) error                 { return nil }
func (BasePlugin) Close() error                                         { return nil }
func (BasePlugin) OnRequestHeaders(Context, extproc.AllHeaders) error   { return nil }
func (BasePlugin) OnRequestBody(Context, []byte) error                  { return nil }
func (BasePlugin) OnRequestTrailers(Context, extproc.AllHeaders) error  { return nil }
func (BasePlugin) OnResponseHeaders(Context, extproc.AllHeaders) error  { return nil }
func (BasePlugin) OnResponseBody(Context, []byte) error                 { return nil }
func (BasePlugin) OnResponseTrailers(Context, extproc.AllHeaders) error { return nil }

// Factory creates a plugin instance.
type Factory func() Plugin

// DynamicLoader starts one dynamic (subprocess) plugin instance bound to cfg.
// Implemented by internal/plugin/dynamic and installed process-wide via
// SetDynamicLoader; the hook indirection keeps this package free of
// exec/OCI/gRPC concerns.
type DynamicLoader interface {
	// Load resolves the plugin binary (pc.URL), spawns it and returns the
	// adapter that forwards phase calls to it. cfg is the instance's merged
	// config node (nil when the plugin has no config section); the subprocess
	// receives it verbatim and runs Init itself.
	Load(pc PluginConfig, cfg *gjson.Json, logger *slog.Logger) (Plugin, error)
}

var dynamicLoader atomic.Pointer[DynamicLoader]

// SetDynamicLoader installs the dynamic plugin loader (called once by boot).
func SetDynamicLoader(l DynamicLoader) { dynamicLoader.Store(&l) }

// loadDynamic resolves a dynamic plugin instance through the installed loader.
func loadDynamic(pc PluginConfig, cfg *gjson.Json, logger *slog.Logger) (Plugin, error) {
	if l := dynamicLoader.Load(); l != nil {
		return (*l).Load(pc, cfg, logger)
	}
	return nil, gerror.Newf("plugin %s sets url %q but no dynamic loader is installed", pc.Name, pc.URL)
}
