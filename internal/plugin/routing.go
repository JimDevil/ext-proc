package plugin

import (
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
)

// RouteTable maps requests to per-plugin chain compositions. It is built once
// per config generation — each plugin slot holds its global instance plus one
// instance per matchRule — and is immutable at runtime: hot reload builds and
// swaps it whole, so request-time composition is plain slice scans with no
// locking.
type RouteTable struct {
	logger   *slog.Logger
	observer Observer
	slots    []pluginSlot
}

// pluginSlot is one plugin's entry in the route table: the orchestration
// entry, the instance built from the global config, and one instance per
// matchRule (the higress wasmPlugin model — each plugin owns its rules
// independently). failOpen is the slot's resolved failure policy (plugin
// override > chain default), shared by the global instance and all variants.
type pluginSlot struct {
	pc       PluginConfig
	failOpen bool
	global   Plugin
	variants []pluginVariant
}

// pluginVariant is one matchRule's instance: the match dimensions plus the
// plugin built from the merged config (or a disable marker).
type pluginVariant struct {
	domains    []string
	pathPrefix string
	disable    bool
	plugin     Plugin
}

// Names returns the plugin names on the table (in chain order).
func (t *RouteTable) Names() []string {
	names := make([]string, 0, len(t.slots))
	for _, s := range t.slots {
		names = append(names, s.pc.Name)
	}
	return names
}

// RuleCount returns the total number of match rules across all plugins.
func (t *RouteTable) RuleCount() int {
	n := 0
	for _, s := range t.slots {
		n += len(s.variants)
	}
	return n
}

// GlobalChain assembles a chain from every slot's global instance — the
// fallback for streams that never went through a request-headers phase.
func (t *RouteTable) GlobalChain() *Chain {
	entries := make([]pluginEntry, 0, len(t.slots))
	for _, s := range t.slots {
		if s.global != nil {
			entries = append(entries, pluginEntry{plugin: s.global, failOpen: s.failOpen})
		}
	}
	return &Chain{entries: entries, logger: t.logger, observer: t.observer}
}

// Compose assembles the per-request chain: every slot resolves its own
// variant (first matching rule wins, fallback to the global instance) and
// contributes its instance — or is skipped when the matching rule disabled
// the plugin for those routes. The result is an immutable Chain; each entry
// carries its own resolved failure policy.
func (t *RouteTable) Compose(authority, path string) *Chain {
	host := strings.ToLower(stripPort(authority))
	entries := make([]pluginEntry, 0, len(t.slots))
	for _, slot := range t.slots {
		if inst, ok := slot.resolve(host, path); ok {
			entries = append(entries, pluginEntry{plugin: inst, failOpen: slot.failOpen})
		}
	}
	return &Chain{entries: entries, logger: t.logger, observer: t.observer}
}

// resolve picks this slot's instance for a request: the first matching rule
// wins (a matching rule with disable=true removes the plugin for the request);
// no match falls back to the global instance.
func (s *pluginSlot) resolve(host, path string) (Plugin, bool) {
	for _, v := range s.variants {
		if ruleMatches(v.domains, v.pathPrefix, host, path) {
			if v.disable {
				return nil, false
			}
			return v.plugin, true
		}
	}
	if s.global == nil {
		return nil, false
	}
	return s.global, true
}

// CloseAll closes every plugin instance on the table (global + all variants).
// Disable-marker variants carry no instance and are skipped. Called by the
// processor when a hot-reload generation retires and its last stream ends, or
// on process shutdown. Plugin.Close must be idempotent.
func (t *RouteTable) CloseAll() {
	logCloseError := func(name string, err error) {
		if t.logger != nil {
			t.logger.Error("failed to close plugin", slog.String("plugin", name), slog.String("error", err.Error()))
		}
	}
	for _, s := range t.slots {
		if s.global != nil {
			if err := s.global.Close(); err != nil {
				logCloseError(s.global.Name(), err)
			}
		}
		for _, v := range s.variants {
			if v.plugin != nil {
				if err := v.plugin.Close(); err != nil {
					logCloseError(v.plugin.Name(), err)
				}
			}
		}
	}
}

// BuildRouteTable builds the route table from the orchestration config:
// for every enabled plugin, one instance is built from the global config
// node, plus one instance per matchRule built from the rule's config
// deep-merged over the global node (a rule with disable: true becomes a
// disable marker instead). Instances are per variant on purpose — different
// routes may run the same plugin with different configs simultaneously.
// Slots are sorted stably by order (ascending; equal orders keep the
// config-file order). An optional observer receives per-plugin metrics for
// every composed chain.
func BuildRouteTable(cfg ChainConfig, tree *gjson.Json, logger *slog.Logger, observers ...Observer) (*RouteTable, error) {
	if logger == nil {
		return nil, gerror.New("cannot build a plugin route table without a logger")
	}
	if _, err := resolveFailOpen("", cfg.FailurePolicy, "chain"); err != nil {
		return nil, err
	}
	var observer Observer
	if len(observers) > 0 {
		observer = observers[0]
	}
	t := &RouteTable{
		logger:   logger,
		observer: observer,
	}

	for i, pc := range cfg.Plugins {
		if !pc.IsEnabled() {
			logger.Debug("plugin disabled, skipping", slog.String("plugin", pc.Name))
			continue
		}
		f, err := mustLookup(pc.Name)
		if err != nil {
			return nil, err
		}

		failOpen, err := resolveFailOpen(pc.FailurePolicy, cfg.FailurePolicy, pc.Name)
		if err != nil {
			return nil, err
		}

		globalNode := pluginConfigNode(tree, i)
		global, err := buildInstance(f, globalNode, pc.Name, logger)
		if err != nil {
			return nil, gerror.Wrapf(err, "plugin %s failed to initialize", pc.Name)
		}
		slot := pluginSlot{pc: pc, failOpen: failOpen, global: global}

		for j, mr := range pc.MatchRules {
			if mr.Disable {
				slot.variants = append(slot.variants, pluginVariant{
					domains: mr.Domain, pathPrefix: mr.PathPrefix, disable: true,
				})
				continue
			}
			merged := mergedConfigNode(globalNode, ruleConfigNode(tree, i, j))
			inst, err := buildInstance(f, merged, pc.Name, logger)
			if err != nil {
				return nil, gerror.Wrapf(err, "plugin %s matchRule #%d failed to initialize", pc.Name, j)
			}
			slot.variants = append(slot.variants, pluginVariant{
				domains: mr.Domain, pathPrefix: mr.PathPrefix, plugin: inst,
			})
		}

		t.slots = append(t.slots, slot)
	}

	sort.SliceStable(t.slots, func(i, j int) bool { return t.slots[i].pc.Order < t.slots[j].pc.Order })

	logger.Info("plugin route table ready",
		slog.Any("plugins", t.Names()), slog.Int("matchRules", t.RuleCount()))
	return t, nil
}

// buildInstance runs the factory and Init with the given config node
// (nil means "no config section" — plugins fall back to their defaults).
func buildInstance(f Factory, cfgNode *gjson.Json, name string, logger *slog.Logger) (Plugin, error) {
	p := f()
	if err := p.Init(cfgNode, logger); err != nil {
		return nil, err
	}
	return p, nil
}

// pluginConfigNode returns plugin i's private config node; nil when absent.
func pluginConfigNode(tree *gjson.Json, pluginIdx int) *gjson.Json {
	if tree == nil {
		return nil
	}
	v := tree.Get("chain.plugins." + strconv.Itoa(pluginIdx) + ".config")
	if v == nil || v.IsNil() {
		return nil
	}
	return gjson.New(v.Val())
}

// ruleConfigNode returns the config override node of matchRule j of plugin i;
// nil when absent.
func ruleConfigNode(tree *gjson.Json, pluginIdx, ruleIdx int) *gjson.Json {
	if tree == nil {
		return nil
	}
	v := tree.Get("chain.plugins." + strconv.Itoa(pluginIdx) + ".matchRules." + strconv.Itoa(ruleIdx) + ".config")
	if v == nil || v.IsNil() {
		return nil
	}
	return gjson.New(v.Val())
}

// mergedConfigNode deep-merges a rule config override over the global config
// node (override wins on scalars, maps recurse). nil-safe on both sides.
func mergedConfigNode(global, override *gjson.Json) *gjson.Json {
	switch {
	case global == nil:
		return override
	case override == nil:
		return global
	}
	return gjson.New(deepMergeMaps(global.Map(), override.Map()))
}

// deepMergeMaps merges src over dst recursively (src wins on scalars; maps
// recurse). Returns a new map; inputs are not modified.
func deepMergeMaps(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if dm, ok := out[k].(map[string]any); ok {
			if sm, ok2 := v.(map[string]any); ok2 {
				out[k] = deepMergeMaps(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ruleMatches reports whether a request matches one rule: every configured
// dimension must match; empty dimensions match everything.
func ruleMatches(domains []string, pathPrefix, host, path string) bool {
	if len(domains) > 0 && !domainMatches(host, domains) {
		return false
	}
	if pathPrefix != "" && !strings.HasPrefix(path, pathPrefix) {
		return false
	}
	return true
}

// domainMatches implements the domain matcher: "*" matches everything,
// "*.example.com" matches any sub-domain of example.com, otherwise exact
// (case-insensitive).
func domainMatches(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		switch {
		case d == "*" || d == host:
			return true
		case strings.HasPrefix(d, "*.") && strings.HasSuffix(host, d[1:]):
			// "*.example.com" matches any name ending in ".example.com"
			return true
		}
	}
	return false
}

// stripPort removes a trailing ":port" from an authority value (bare IPv6
// literals have no port and pass through unchanged).
func stripPort(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return authority
}
