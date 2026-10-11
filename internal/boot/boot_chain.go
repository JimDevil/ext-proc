package boot

import (
	"context"
	"log/slog"

	"github.com/JimDevil/ext-proc/internal/model/config"
	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/processor"
)

// InitChain builds the plugin route table (global chain + matchRule variants)
// and registers the global processor, plus the hot-reload hook: when the gcfg
// watcher detects a config change the table is rebuilt wholesale and swapped
// atomically, without restarting the process.
func InitChain(ctx context.Context) error {
	proc := processor.New(config.Cfg.App.Name, config.Log)

	rebuild := func(ctx context.Context) error {
		table, err := plugin.BuildRouteTable(config.Cfg.Chain, config.Raw, config.Log)
		if err != nil {
			return err
		}
		// Atomic swap: in-flight streams keep the old table, new traffic uses
		// the new one.
		proc.SwapChain(table)
		config.Log.Info("plugin route table ready",
			slog.Any("plugins", table.Names()), slog.Int("matchRules", table.RuleCount()))
		return nil
	}

	if err := rebuild(ctx); err != nil {
		return err
	}

	// Register the hot-reload hook (invoked by gcfg's OnChange).
	config.OnConfigReload = rebuild
	processor.Register(proc)
	return nil
}
