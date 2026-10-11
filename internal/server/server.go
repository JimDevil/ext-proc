// Package server assembles the ext_proc process: the SDK's gRPC service +
// Prometheus metrics port + graceful shutdown.
package server

import (
	"context"
	"log/slog"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/model/config"
	"github.com/JimDevil/ext-proc/internal/processor"
)

// Run runs the ext_proc gRPC service (blocking); SIGTERM/SIGINT trigger the
// SDK's built-in graceful shutdown (drain -> propagation delay -> bounded
// GracefulStop -> forced stop).
func Run(ctx context.Context) error {
	proc := processor.Global()
	if proc == nil {
		return gerror.New("chain processor not initialized; make sure boot.Init succeeds before server.Run")
	}

	s := config.Cfg.Server
	opts := extproc.NewDefaultServerOptions()
	opts.ExtProcPort = s.ExtProcPort
	opts.MetricsHTTPPort = s.MetricsPort
	opts.MaxConcurrentStreams = s.MaxConcurrentStreams
	opts.UnreadyPropagationDelaySeconds = s.UnreadyPropagationDelaySeconds
	opts.TerminationGracePeriodSeconds = s.TerminationGracePeriodSeconds

	config.Log.Info("starting ext_proc service",
		slog.Int("extProcPort", s.ExtProcPort),
		slog.Int("metricsPort", s.MetricsPort),
		slog.Uint64("maxConcurrentStreams", uint64(s.MaxConcurrentStreams)))

	// Serve handles signals and graceful shutdown internally; returning means
	// the process should exit.
	if err := extproc.Serve(opts, proc, config.Log); err != nil {
		return gerror.Wrap(err, "ext_proc service exited unexpectedly")
	}
	return nil
}
