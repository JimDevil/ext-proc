package boot

import (
	"context"
	"log/slog"

	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/model/config"
	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/processor"
)

// InitChain builds the initial plugin route table and registers the
// transactional hot-reload hook. The hook validates the next generation,
// builds the whole table, swaps it atomically and only then publishes the new
// config snapshot. A failed rebuild leaves the previous generation intact.
func InitChain(ctx context.Context) error {
	snap := config.Current()
	if snap.Log == nil {
		return gerror.New("logger is not initialized")
	}
	if snap.Raw == nil {
		return gerror.New("raw config tree is not initialized")
	}

	proc := processor.New(snap.Cfg.App.Name, snap.Log)
	table, err := plugin.BuildRouteTable(snap.Cfg.Chain, snap.Raw, snap.Log, proc.Observer())
	if err != nil {
		return gerror.Wrap(err, "build initial plugin route table")
	}
	proc.SwapChain(table)

	config.SetReloadHook(func(ctx context.Context, next *config.Snapshot) error {
		if next == nil {
			return gerror.New("reload snapshot is nil")
		}
		old := config.Current()
		if err := config.ValidateReloadable(old.Cfg, next.Cfg); err != nil {
			return err
		}
		if next.Raw == nil {
			return gerror.New("reload snapshot has no raw config tree")
		}
		next.Log = old.Log
		table, err := plugin.BuildRouteTable(next.Cfg.Chain, next.Raw, next.Log, proc.Observer())
		if err != nil {
			return gerror.Wrap(err, "build reloaded plugin route table")
		}
		// The table is immutable and swapped atomically; in-flight streams
		// keep the old table and its plugin instances until they finish.
		proc.SwapChain(table)
		config.Store(next)
		next.Log.Info("config reloaded",
			slog.Any("plugins", table.Names()), slog.Int("matchRules", table.RuleCount()))
		return nil
	})

	processor.Register(proc)
	return nil
}
