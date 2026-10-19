package server

import (
	"context"
	"testing"

	"github.com/JimDevil/ext-proc/internal/model/config"
)

func TestServerOptionsFromConfig(t *testing.T) {
	t.Parallel()

	cfg := config.ServerConfig{
		BindAddress:                    "127.0.0.1",
		MetricsBindAddress:             "127.0.0.1",
		ExtProcPort:                    50051,
		MetricsPort:                    19090,
		MaxConcurrentStreams:           1234,
		MaxRecvMsgSize:                 8 << 20,
		MaxSendMsgSize:                 9 << 20,
		UnreadyPropagationDelaySeconds: 7,
		TerminationGracePeriodSeconds:  21,
		TLS: config.TLSConfig{
			Enabled:      true,
			CertFile:     "/tls/tls.crt",
			KeyFile:      "/tls/tls.key",
			ClientCAFile: "/tls/ca.crt",
			MinVersion:   "1.3",
		},
	}
	opts := serverOptionsFromConfig(cfg)
	if opts.BindAddress != cfg.BindAddress || opts.MetricsBindAddress != cfg.MetricsBindAddress {
		t.Fatalf("bind address mapping mismatch: %+v", opts)
	}
	if opts.ExtProcPort != cfg.ExtProcPort || opts.MetricsHTTPPort != cfg.MetricsPort {
		t.Fatalf("port mapping mismatch: %+v", opts)
	}
	if opts.MaxConcurrentStreams != cfg.MaxConcurrentStreams ||
		opts.MaxRecvMsgSize != cfg.MaxRecvMsgSize ||
		opts.MaxSendMsgSize != cfg.MaxSendMsgSize ||
		opts.UnreadyPropagationDelaySeconds != cfg.UnreadyPropagationDelaySeconds ||
		opts.TerminationGracePeriodSeconds != cfg.TerminationGracePeriodSeconds {
		t.Fatalf("limit mapping mismatch: %+v", opts)
	}
	if opts.TLS == nil || opts.TLS.CertFile != cfg.TLS.CertFile || opts.TLS.ClientCAFile != cfg.TLS.ClientCAFile || opts.TLS.MinVersion != "1.3" {
		t.Fatalf("TLS mapping mismatch: %+v", opts.TLS)
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("mapped options must validate: %v", err)
	}
}

func TestRunRequiresProcessor(t *testing.T) {
	t.Parallel()

	if err := Run(context.Background()); err == nil {
		t.Fatal("Run must fail when the chain processor was not initialized")
	}
}
