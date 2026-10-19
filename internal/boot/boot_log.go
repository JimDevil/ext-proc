package boot

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/JimDevil/ext-proc/internal/model/config"
)

// InitLog initializes the global slog logger from config (JSON structured
// output to stderr). Runtime code uses config.Current().Log / slog.Default()
// only, so structured logs flow straight into the collection pipeline.
func InitLog(ctx context.Context) error {
	snap := config.Current()
	level := slog.LevelInfo
	switch strings.ToLower(snap.Cfg.Logger.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	config.SetLogger(logger)
	slog.SetDefault(logger)
	logger.Info("config loaded",
		slog.String("app", snap.Cfg.App.Name),
		slog.String("version", snap.Cfg.App.Version))
	return nil
}
