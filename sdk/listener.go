package extproc

import (
	"log/slog"

	epb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// AssembleExtProcServer builds a gRPC server with the ext_proc ExternalProcessor
// service and the standard health service registered, wired to processor.
// The caller serves it on any listener it owns (tcp, unix, or an in-process
// pipe for tests) and owns the whole lifecycle: shutdown, draining and
// processor.Close are NOT handled here.
//
// This is the embedding entry point — used by the subprocess plugin runtime
// (internal/pluginhost) and by tests; Serve is the full-service variant that
// adds the metrics endpoint, signal handling and graceful shutdown.
func AssembleExtProcServer(serverOptions *ServerOptions, processor RequestProcessor, logger *slog.Logger) *grpc.Server {
	if processor == nil {
		logger.Error("cannot process request stream without `processor`")
		return nil
	}

	// Dedicated registry (not prometheus.DefaultRegisterer) so several
	// servers can coexist in one process; without an HTTP endpoint the
	// metrics are simply never exposed.
	registry := prometheus.NewRegistry()
	metrics := NewEmptyMetrics().Register(registry)

	sopts := []grpc.ServerOption{grpc.MaxConcurrentStreams(serverOptions.MaxConcurrentStreams)}
	s := grpc.NewServer(sopts...)

	name := processor.GetName()
	opts := processor.GetOptions()
	if opts == nil {
		// Defense: fall back to defaults when the processor forgets its
		// options, avoiding a nil dereference later.
		opts = NewDefaultProcessingOptions()
	}
	extprocServer := &GenericExtProcServer{
		name:      name,
		processor: processor,
		options:   opts,
		metrics:   metrics,
		logger:    logger,
	}
	epb.RegisterExternalProcessorServer(s, extprocServer)

	health := NewReadyHealthServer()
	hpb.RegisterHealthServer(s, health)

	return s
}
