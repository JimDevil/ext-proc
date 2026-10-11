package plugin

import (
	"log/slog"
	"sort"
	"sync"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register self-registers a plugin (called from the plugin package's init()).
// Duplicate registration panics: the plugin name is the config-orchestration
// key, and a duplicate signals an integration bug — fail fast beats silently
// overwriting.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(gerror.Newf("plugin %s registered twice", name))
	}
	registry[name] = f
}

// mustLookup finds a plugin factory by name.
func mustLookup(name string) (Factory, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	if !ok {
		return nil, gerror.Newf("plugin %s is not registered; make sure main.go anonymous-imports its package", name)
	}
	return f, nil
}

// Lookup finds a plugin factory by name (false when not registered). Used by
// the subprocess plugin runtime (internal/pluginhost) to build the single
// plugin a dynamic plugin process serves.
func Lookup(name string) (Factory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// MustLookup is Lookup with a panic on absence — for plugin binaries whose
// main() blank-imports exactly one plugin package.
func MustLookup(name string) Factory {
	f, ok := Lookup(name)
	if !ok {
		panic(gerror.Newf("plugin %s is not registered; make sure main.go anonymous-imports its package", name))
	}
	return f
}

// BuildChain builds a single chain from the orchestration config:
//  1. filters out plugins with enabled=false;
//  2. creates instances from the registered factories and calls Init with the
//     config node extracted from the raw config tree by plugin name;
//  3. sorts stably by order (equal orders keep the config-file order).
//
// tree may be nil (unit-test scenarios); plugins then receive a nil config
// node and fall back to their own defaults. See BuildRouteTable for the
// route-aware variant.
func BuildChain(cfg ChainConfig, tree *gjson.Json, logger *slog.Logger) (*Chain, error) {
	return buildOneChain(cfg.Plugins, cfg.FailurePolicy, func(name string) *gjson.Json {
		return pluginConfigNodeByName(tree, name)
	}, logger)
}

// buildOneChain is the shared chain constructor. configFor resolves the
// plugin-private config node per plugin name — BuildChain reads it from the
// raw tree; route variants hand out merged nodes instead. failurePolicy is
// the chain-level default that plugins may override individually.
func buildOneChain(pcs []PluginConfig, failurePolicy string, configFor func(name string) *gjson.Json, logger *slog.Logger) (*Chain, error) {
	type item struct {
		p        Plugin
		order    int
		failOpen bool
	}

	var items []item

	for _, pc := range pcs {
		if !pc.Enabled {
			logger.Info("plugin disabled, skipping", slog.String("plugin", pc.Name))
			continue
		}
		failOpen, err := resolveFailOpen(pc.FailurePolicy, failurePolicy, pc.Name)
		if err != nil {
			return nil, err
		}
		f, err := mustLookup(pc.Name)
		if err != nil {
			return nil, err
		}
		p := f()
		if err := p.Init(configFor(pc.Name), logger); err != nil {
			return nil, gerror.Wrapf(err, "plugin %s failed to initialize", pc.Name)
		}
		items = append(items, item{p: p, order: pc.Order, failOpen: failOpen})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].order < items[j].order })

	entries := make([]pluginEntry, 0, len(items))
	names := make([]string, 0, len(items))
	for _, it := range items {
		entries = append(entries, pluginEntry{plugin: it.p, failOpen: it.failOpen})
		names = append(names, it.p.Name())
	}
	logger.Info("plugin chain built",
		slog.String("failurePolicy", failurePolicy), slog.Any("plugins", names))

	return &Chain{entries: entries, logger: logger}, nil
}

// resolveFailOpen resolves the effective failure policy for one plugin:
// a plugin-level override wins over the chain-level default. Both levels are
// validated — an unknown value fails the build instead of silently meaning
// fail_closed.
func resolveFailOpen(pluginPolicy, chainPolicy, pluginName string) (bool, error) {
	switch pluginPolicy {
	case "":
		// Inherit the chain-level default (empty chain policy = fail_closed).
		switch chainPolicy {
		case "", "fail_closed":
			return false, nil
		case "fail_open":
			return true, nil
		default:
			return false, gerror.Newf("plugin %s: chain.failurePolicy 非法: %q（仅支持 fail_open / fail_closed，留空为 fail_closed）", pluginName, chainPolicy)
		}
	case "fail_open":
		return true, nil
	case "fail_closed":
		return false, nil
	default:
		return false, gerror.Newf("plugin %s: failurePolicy 非法: %q（仅支持 fail_open / fail_closed，留空继承链级）", pluginName, pluginPolicy)
	}
}

// pluginConfigNodeByName finds a plugin's config node in the full config tree
// by plugin name; nil when absent. (BuildRouteTable uses the index-based
// pluginConfigNode in routing.go instead — matchRules live under the plugin
// entry, keyed by position.)
func pluginConfigNodeByName(tree *gjson.Json, name string) *gjson.Json {
	if tree == nil {
		return nil
	}
	for _, item := range tree.GetJsons("chain.plugins") {
		if item.Get("name").String() == name {
			v := item.Get("config")
			if v == nil || v.IsNil() {
				return nil
			}
			return gjson.New(v.Val())
		}
	}
	return nil
}
