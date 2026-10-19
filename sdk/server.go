package extproc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Serve assembles and runs the ext_proc service with a fresh metric set.
// See ServeWithMetrics for the full contract.
func Serve(serverOptions *ServerOptions, processor RequestProcessor, logger *slog.Logger) error {
	return ServeWithMetrics(serverOptions, processor, logger, NewEmptyMetrics())
}

// ServeWithMetrics assembles and runs the ext_proc service (blocking):
//
//  1. a dedicated HTTP metrics/health port (/metrics, /healthz, /readyz);
//  2. the gRPC service (ExternalProcessor + health check), optionally TLS/mTLS;
//  3. three-step graceful shutdown on SIGTERM/SIGINT:
//     mark health unready -> wait the propagation delay -> bounded
//     GracefulStop (forced Stop past the deadline) -> metrics shutdown ->
//     processor Close.
//
// metrics must be a fresh, unregistered set (for example, the one passed to a
// processor's SetMetrics so plugin-level metrics share the same registry).
func ServeWithMetrics(serverOptions *ServerOptions, processor RequestProcessor, logger *slog.Logger, metrics *Metrics) error {
	if logger == nil {
		logger = slog.Default()
	}
	if processor == nil {
		logger.Error("cannot process request stream without `processor`")
		return fmt.Errorf("processor is nil")
	}
	if serverOptions == nil {
		return fmt.Errorf("server options are nil")
	}
	if err := serverOptions.Validate(); err != nil {
		logger.Error("invalid server options", slog.String("error", err.Error()))
		return err
	}

	if metrics == nil {
		metrics = NewEmptyMetrics()
	}
	registry := prometheus.NewRegistry()
	metrics = metrics.Register(registry)
	health := NewReadyHealthServer()

	// Metrics server: dedicated mux (not http.DefaultServeMux) so that Serve
	// can be called more than once in a process without panicking. Bind the
	// listener synchronously so a port conflict fails startup loudly instead
	// of leaving the process running without observability.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	))
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	metricsMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if health.GetStatus() == hpb.HealthCheckResponse_SERVING {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining\n"))
	})
	metricsAddr := net.JoinHostPort(serverOptions.MetricsBindAddress, strconv.Itoa(serverOptions.MetricsHTTPPort))
	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsListener, err := net.Listen("tcp", metricsAddr)
	if err != nil {
		logger.Error("Failed to listen on metrics port", slog.String("addr", metricsAddr), slog.String("error", err.Error()))
		return err
	}
	defer metricsServer.Close()
	go func() {
		logger.Info("Started metrics HTTP server", slog.String("addr", metricsAddr))
		if err := metricsServer.Serve(metricsListener); err != nil && err != http.ErrServerClosed {
			logger.Error("Failed to serve metrics HTTP", slog.String("error", err.Error()))
		}
	}()

	// Load TLS material before opening the gRPC listener so a bad certificate
	// cannot leave a listener behind. The stop function tears down the
	// certificate reloader on every return path.
	var tlsConfig *tls.Config
	stopTLS := func() {}
	if serverOptions.TLS != nil {
		tlsConfig, stopTLS, err = newTLSConfig(serverOptions.TLS, logger)
		if err != nil {
			logger.Error("Failed to load TLS configuration", slog.String("error", err.Error()))
			return err
		}
		defer stopTLS()
	}

	// Assemble and register the ext_proc gRPC service.
	grpcAddr := net.JoinHostPort(serverOptions.BindAddress, strconv.Itoa(serverOptions.ExtProcPort))
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		logger.Error("Failed to listen", slog.String("addr", grpcAddr), slog.String("error", err.Error()))
		return err
	}
	if tlsConfig == nil && !isLoopbackBind(serverOptions.BindAddress) {
		logger.Warn("ext_proc gRPC is listening without TLS on a non-loopback address; use TLS for cross-pod deployments",
			slog.String("addr", grpcAddr))
	}

	sopts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(serverOptions.MaxConcurrentStreams),
		grpc.MaxRecvMsgSize(serverOptions.MaxRecvMsgSize),
		grpc.MaxSendMsgSize(serverOptions.MaxSendMsgSize),
	}
	if tlsConfig != nil {
		sopts = append(sopts, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}
	s := grpc.NewServer(sopts...)

	name := processor.GetName()
	opts := processor.GetOptions()
	if opts == nil {
		// Defense: fall back to defaults when the processor forgets its
		// options, avoiding a nil dereference later.
		opts = NewDefaultProcessingOptions()
	}
	extproc := &GenericExtProcServer{
		name:      name,
		processor: processor,
		options:   opts,
		metrics:   metrics,
		logger:    logger,
	}
	epb.RegisterExternalProcessorServer(s, extproc)
	hpb.RegisterHealthServer(s, health)

	logger.Info("Starting extproc gRPC Server",
		slog.String("name", name),
		slog.String("addr", grpcAddr),
		slog.Bool("tls", tlsConfig != nil),
		slog.Bool("mtls", tlsConfig != nil && tlsConfig.ClientAuth == tls.RequireAndVerifyClientCert))

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(lis) }()

	gracefulStop := make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(gracefulStop)

	grace := time.Duration(serverOptions.TerminationGracePeriodSeconds) * time.Second

	select {
	case err := <-serveErr:
		// Serve returns nil only after Stop/GracefulStop, neither of which can
		// have run before a signal; guard anyway so a future change can't panic.
		if err == nil {
			logger.Warn("gRPC server exited without error before shutdown was requested")
			return nil
		}
		logger.Error("gRPC server exited unexpectedly", slog.String("error", err.Error()))
		return err

	case sig := <-gracefulStop:

		logger.Info("Caught signal, waiting to finish processing",
			slog.String("signal", sig.String()),
			slog.Duration("grace_period", grace),
			slog.Int("grace_period_seconds", int(serverOptions.TerminationGracePeriodSeconds)))

		// 1. Drain: mark the health check unready (HTTP /readyz starts
		// returning 503 immediately).
		health.MarkUnready()

		// 2. Wait for lb/probe to observe the unready state.
		time.Sleep(time.Duration(serverOptions.UnreadyPropagationDelaySeconds) * time.Second)

		// 3. GOAWAY and wait for in-flight streams, bounded by the grace period.
		stopped := make(chan struct{})
		go func() {
			s.GracefulStop() // closes lis for us; do NOT call lis.Close()
			close(stopped)
		}()

		select {
		case <-stopped:
			logger.Info("all active streams drained")
		case <-time.After(grace):
			logger.Warn("grace period elapsed, forcing stop")
			s.Stop() // NOTE: aborts remaining streams
			<-stopped
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics HTTP server shutdown error", slog.String("error", err.Error()))
		}

		if err := extproc.Close(serverOptions.TerminationGracePeriodSeconds); err != nil {
			logger.Error("processor close error", slog.String("error", err.Error()))
			return err
		}
		return nil
	}
}

// MustServe is the Must variant of Serve: exits the process on error.
func MustServe(serverOptions *ServerOptions, processor RequestProcessor, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := Serve(serverOptions, processor, logger); err != nil {
		logger.Error("extproc server failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
