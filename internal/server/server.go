// Package server assembles the ext_proc process: the SDK's gRPC service +
// Prometheus metrics/health port + graceful shutdown.
package server

import (
	"context"
	"log/slog"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/model/config"
	"github.com/JimDevil/ext-proc/internal/processor"
)

// serverOptionsFromConfig maps the validated config section to the SDK's
// server options. It is kept separate so the mapping can be unit tested.
func serverOptionsFromConfig(s config.ServerConfig) *extproc.ServerOptions {
	opts := extproc.NewDefaultServerOptions()
	opts.BindAddress = s.BindAddress
	opts.MetricsBindAddress = s.MetricsBindAddress
	opts.ExtProcPort = s.ExtProcPort
	opts.MetricsHTTPPort = s.MetricsPort
	opts.MaxConcurrentStreams = s.MaxConcurrentStreams
	opts.MaxRecvMsgSize = s.MaxRecvMsgSize
	opts.MaxSendMsgSize = s.MaxSendMsgSize
	opts.UnreadyPropagationDelaySeconds = s.UnreadyPropagationDelaySeconds
	opts.TerminationGracePeriodSeconds = s.TerminationGracePeriodSeconds
	if s.TLS.Enabled {
		opts.TLS = &extproc.TLSOptions{
			CertFile:              s.TLS.CertFile,
			KeyFile:               s.TLS.KeyFile,
			ClientCAFile:          s.TLS.ClientCAFile,
			MinVersion:            s.TLS.MinVersion,
			ReloadIntervalSeconds: s.TLS.ReloadIntervalSeconds,
		}
	}
	return opts
}

// Run runs the ext_proc gRPC service (blocking); SIGTERM/SIGINT trigger the
// SDK's built-in graceful shutdown (drain -> propagation delay -> bounded
// GracefulStop -> forced stop).
func Run(ctx context.Context) error {
	proc := processor.Global()
	if proc == nil {
		return gerror.New("chain processor not initialized; make sure boot.Init succeeds before server.Run")
	}

	snap := config.Current()
	if snap.Log == nil {
		return gerror.New("logger is not initialized")
	}

	s := snap.Cfg.Server
	opts := serverOptionsFromConfig(s)

	snap.Log.Info("starting ext_proc service",
		slog.String("bindAddress", s.BindAddress),
		slog.Int("extProcPort", s.ExtProcPort),
		slog.String("metricsBindAddress", s.MetricsBindAddress),
		slog.Int("metricsPort", s.MetricsPort),
		slog.Uint64("maxConcurrentStreams", uint64(s.MaxConcurrentStreams)),
		slog.Bool("tls", s.TLS.Enabled),
		slog.Bool("mtls", s.TLS.Enabled && s.TLS.ClientCAFile != ""))

	// The metrics set is created here so plugin-level metrics and the HTTP
	// registry share the same collectors.
	metrics := extproc.NewEmptyMetrics()
	proc.SetMetrics(metrics)

	// Serve handles signals and graceful shutdown internally; returning means
	// the process should exit.
	if err := extproc.ServeWithMetrics(opts, proc, snap.Log, metrics); err != nil {
		return gerror.Wrap(err, "ext_proc service exited unexpectedly")
	}
	return nil
}
