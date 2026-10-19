package boot

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"reflect"
	"strings"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"
	"github.com/gogf/gf/v2/os/gcfg"
	"github.com/gogf/gf/v2/util/gconv"

	"github.com/JimDevil/ext-proc/internal/model/config"
)

// Package-level source state: InitConfig performs the initial load,
// InitWatch starts the watcher after the chain and reload hook are ready.
var (
	cfgLoader  *gcfg.Loader[config.Config]
	cfgAdapter *gcfg.AdapterFile
	cfgRawTree *gjson.Json
	cfgRawJSON []byte
)

// InitConfig loads and validates the configuration once. The watcher is
// started later by InitWatch so the reload hook is registered before any
// change event can be delivered.
//
// EXT_PROC_CONFIG overrides the file name/path (gcfg otherwise searches the
// conventional manifest/config directory).
func InitConfig(ctx context.Context) error {
	fileName := "config.yaml"
	if override := strings.TrimSpace(os.Getenv("EXT_PROC_CONFIG")); override != "" {
		fileName = override
	}

	adapter, err := gcfg.NewAdapterFile(fileName)
	if err != nil {
		return gerror.Wrap(err, "create config adapter")
	}

	loader := gcfg.NewLoaderWithAdapter[config.Config](adapter, "")
	loader.SetConverter(func(data any, target *config.Config) error {
		if err := gconv.Scan(data, target); err != nil {
			return gerror.Wrap(err, "decode typed config")
		}
		// The converter runs inside the loader's locked Load call, before
		// OnChange. Updating the package-level tree here keeps the raw plugin
		// configs of a reload tied to the same file read as the typed config.
		tree := gjson.New(data)
		encoded, err := tree.ToJson()
		if err != nil {
			return gerror.Wrap(err, "encode raw config tree")
		}
		cfgRawTree = tree
		cfgRawJSON = encoded
		return nil
	})

	if err := loader.Load(ctx); err != nil {
		return gerror.Wrap(err, "load config")
	}

	cfg := loader.Get()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return gerror.Wrap(err, "validate config")
	}
	if cfgRawTree == nil || len(cfgRawJSON) == 0 {
		return gerror.New("raw config tree is empty")
	}

	config.Store(&config.Snapshot{Cfg: cfg, Raw: cfgRawTree, RawJSON: cfgRawJSON})
	cfgLoader = loader
	cfgAdapter = adapter
	return nil
}

// InitWatch starts watching the config file. The typed loader updates its own
// target first; the callback then builds a complete next snapshot and lets the
// chain swap it in. If the rebuild fails, the previously published snapshot
// (and therefore the old chain) stays active.
func InitWatch(ctx context.Context) error {
	if cfgLoader == nil {
		return gerror.New("config loader is not initialized")
	}
	logger := config.Current().Log
	if logger == nil {
		return gerror.New("logger is not initialized")
	}

	cfgLoader.OnChange(func(updated config.Config) error {
		raw := cfgRawTree
		if raw == nil {
			return gerror.New("raw config tree is empty")
		}
		updated.ApplyDefaults()
		if err := updated.Validate(); err != nil {
			return err
		}
		// The file watcher can deliver more than one event for a single save
		// (write/chmod/rename). Coalesce them so one edit produces one chain
		// generation instead of rebuilding identical plugin instances twice.
		// Compare the raw tree too: plugin-private config is not part of the
		// typed struct.
		current := config.Current()
		if reflect.DeepEqual(updated, current.Cfg) && bytes.Equal(cfgRawJSON, current.RawJSON) {
			return nil
		}
		next := &config.Snapshot{Cfg: updated, Raw: raw, RawJSON: cfgRawJSON, Log: logger}
		if err := config.ApplyReload(ctx, next); err != nil {
			return err
		}
		return nil
	})
	cfgLoader.SetWatchErrorHandler(func(ctx context.Context, err error) {
		logger.Error("config watcher reload failed; keeping the previous generation", slog.String("error", err.Error()))
	})

	if err := cfgLoader.Watch(ctx, "app-watcher"); err != nil {
		return gerror.Wrap(err, "start config watcher")
	}
	logger.Info("config watcher started")
	return nil
}
