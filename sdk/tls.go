package extproc

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// loadTLSConfig builds a static TLS configuration (no certificate reload).
// It is kept for callers that only need the material once; the server uses
// newTLSConfig so certificates can be rotated without a restart.
func loadTLSConfig(opts *TLSOptions) (*tls.Config, error) {
	cfg, _, err := loadTLSMaterial(opts)
	return cfg, err
}

// loadTLSMaterial reads the certificate/CA files and builds a TLS config. The
// returned digest covers all input files so the reloader can skip swapping
// (and logging) when nothing changed.
func loadTLSMaterial(opts *TLSOptions) (*tls.Config, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if opts == nil {
		return nil, digest, nil
	}
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, digest, fmt.Errorf("TLS cert_file and key_file are required")
	}
	certPEM, err := os.ReadFile(opts.CertFile)
	if err != nil {
		return nil, digest, fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(opts.KeyFile)
	if err != nil {
		return nil, digest, fmt.Errorf("read TLS private key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, digest, fmt.Errorf("load TLS key pair: %w", err)
	}

	minVersion, err := tlsMinVersion(opts.MinVersion)
	if err != nil {
		return nil, digest, err
	}
	h := sha256.New()
	_, _ = h.Write(certPEM)
	_, _ = h.Write(keyPEM)

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
	}
	if opts.ClientCAFile != "" {
		pem, err := os.ReadFile(opts.ClientCAFile)
		if err != nil {
			return nil, digest, fmt.Errorf("read TLS client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, digest, fmt.Errorf("TLS client CA file %q contains no valid certificates", opts.ClientCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		_, _ = h.Write(pem)
	}
	copy(digest[:], h.Sum(nil))
	return cfg, digest, nil
}

func tlsMinVersion(raw string) (uint16, error) {
	switch raw {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("invalid TLS min_version %q (want 1.2 or 1.3)", raw)
	}
}

// tlsReloader owns the current TLS material and swaps it atomically. When a
// reload interval is configured, a single background goroutine polls the
// files; a failed reload keeps the previous material, so a half-written
// Secret never breaks live connections.
type tlsReloader struct {
	opts   *TLSOptions
	logger *slog.Logger

	mu     sync.RWMutex
	cfg    *tls.Config
	digest [sha256.Size]byte

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// newTLSConfig returns a TLS config whose GetConfigForClient serves the
// current material, plus a stop function that must be called when the server
// shuts down.
func newTLSConfig(opts *TLSOptions, logger *slog.Logger) (*tls.Config, func(), error) {
	if opts == nil {
		return nil, func() {}, nil
	}
	r, err := newTLSReloader(opts, logger)
	if err != nil {
		return nil, nil, err
	}
	if opts.ReloadIntervalSeconds > 0 {
		r.done = make(chan struct{})
		go r.watch(time.Duration(opts.ReloadIntervalSeconds) * time.Second)
	}
	base := &tls.Config{GetConfigForClient: r.getConfig}
	return base, r.close, nil
}

func newTLSReloader(opts *TLSOptions, logger *slog.Logger) (*tlsReloader, error) {
	if logger == nil {
		logger = slog.Default()
	}
	material, digest, err := loadTLSMaterial(opts)
	if err != nil {
		return nil, err
	}
	return &tlsReloader{
		opts:   opts,
		logger: logger,
		cfg:    material,
		digest: digest,
		stop:   make(chan struct{}),
	}, nil
}

func (r *tlsReloader) getConfig(*tls.ClientHelloInfo) (*tls.Config, error) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	return cfg, nil
}

func (r *tlsReloader) watch(interval time.Duration) {
	defer close(r.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			if err := r.reload(); err != nil {
				r.logger.Error("TLS certificate reload failed; keeping the previous material",
					slog.String("error", err.Error()))
			}
		}
	}
}

func (r *tlsReloader) reload() error {
	cfg, digest, err := loadTLSMaterial(r.opts)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if digest == r.digest {
		return nil
	}
	r.cfg = cfg
	r.digest = digest
	r.logger.Info("TLS certificate material reloaded")
	return nil
}

func (r *tlsReloader) close() {
	r.stopOnce.Do(func() {
		close(r.stop)
		if r.done != nil {
			<-r.done
		}
	})
}

// isLoopbackBind reports whether bindAddress only accepts local connections.
// An empty address means all interfaces and is therefore not loopback.
func isLoopbackBind(bindAddress string) bool {
	addr := strings.TrimSpace(bindAddress)
	if addr == "" {
		return false
	}
	if strings.EqualFold(addr, "localhost") {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}
