package plugin

import (
	"sync"

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
			return false, gerror.Newf("plugin %s: invalid chain failurePolicy %q (want fail_open/fail_closed; empty means fail_closed)", pluginName, chainPolicy)
		}
	case "fail_open":
		return true, nil
	case "fail_closed":
		return false, nil
	default:
		return false, gerror.Newf("plugin %s: invalid failurePolicy %q (want fail_open/fail_closed; empty inherits the chain default)", pluginName, pluginPolicy)
	}
}
