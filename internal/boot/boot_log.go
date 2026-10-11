package boot

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/JimDevil/ext-proc/internal/model/config"
)

// InitLog initializes the global slog logger from config (JSON structured
// output to stderr). Runtime code uses config.Log / slog.Default() only —
// never GoFrame's g.Log() — so structured logs flow straight into the
// collection pipeline (data-plane component).
func InitLog(ctx context.Context) error {
	level := slog.LevelInfo
	switch strings.ToLower(config.Cfg.Logger.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	config.Log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(config.Log)
	return nil
}
