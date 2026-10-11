package boot

import (
	"context"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/os/gcfg"

	"github.com/JimDevil/ext-proc/internal/model/config"
)

// InitConfig loads manifest/config/config.yaml with GoFrame's gcfg and watches
// it for hot reload:
//   - the typed Config is written back to config.Cfg (snapshot replaced whole);
//   - the raw config tree goes to config.Raw (plugin-private config sections
//     are extracted from here);
//   - the change callback updates the snapshot first, then triggers the chain
//     rebuild hook.
func InitConfig(ctx context.Context) error {
	// Dedicated config-file adapter (gcfg searches manifest/config/config.yaml
	// per the GoFrame convention).
	adapter, err := gcfg.NewAdapterFile("config.yaml")
	if err != nil {
		return err
	}

	// The generic loader deserializes the config into the typed struct.
	loader := gcfg.NewLoaderWithAdapter[config.Config](adapter, "")

	// The config object shares the adapter with the loader and serves the raw
	// tree (plugin-private config sections).
	cfgObj := gcfg.NewWithAdapter(adapter)

	loader.OnChange(func(updated config.Config) error {
		// Update the global snapshot before triggering the rebuild so the
		// chain is built from the new config.
		config.Raw = gjson.New(cfgObj.MustGet(ctx, ".").Val())
		config.Cfg = updated
		if config.OnConfigReload != nil {
			return config.OnConfigReload(ctx)
		}
		if config.Log != nil {
			config.Log.Info("config hot-reloaded; chain rebuild hook not registered yet, skipping rebuild")
		}
		return nil
	})

	// Load and watch the config file.
	loader.MustLoadAndWatch(ctx, "app-watcher")

	config.Cfg = loader.Get()
	config.Raw = gjson.New(cfgObj.MustGet(ctx, ".").Val())
	g.Log().Infof(ctx, "config loaded: app=%s version=%s", config.Cfg.App.Name, config.Cfg.App.Version)
	return nil
}
