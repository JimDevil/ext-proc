// Package config defines the global config structs and holds the immutable
// runtime snapshot (typed config + raw config tree + logger). Boot loads and
// validates a snapshot; hot reload builds a new one and swaps it atomically.
package config

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gogf/gf/v2/encoding/gjson"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// Snapshot is one immutable configuration generation. Hot reload builds a
// complete replacement and publishes it with a single Store, so request
// goroutines always observe a consistent (Cfg, Raw, Log) triple.
type Snapshot struct {
	Cfg Config
	Raw *gjson.Json
	// RawJSON is the serialized raw tree from the same file read as Cfg. It
	// lets hot reload detect changes in plugin-private config that the typed
	// struct does not model.
	RawJSON []byte
	Log     *slog.Logger
}

var current atomic.Pointer[Snapshot]

// Current returns the active snapshot. The returned value must be treated as
// read-only. Before boot has published a snapshot it returns an empty one.
func Current() *Snapshot {
	if s := current.Load(); s != nil {
		return s
	}
	return &Snapshot{}
}

// Store atomically publishes a new snapshot.
func Store(s *Snapshot) {
	if s != nil {
		current.Store(s)
	}
}

// SetLogger installs the process logger into the current snapshot. Logging
// configuration is not hot-reloadable, so this only happens during boot.
func SetLogger(logger *slog.Logger) {
	old := Current()
	next := *old
	next.Log = logger
	Store(&next)
}

// ReloadHook builds and publishes a new generation. It must only commit the
// snapshot after the new plugin chain was built successfully.
type ReloadHook func(ctx context.Context, next *Snapshot) error

var (
	reloadMu   sync.RWMutex
	reloadHook ReloadHook
)

// SetReloadHook registers the hot-reload handler. It is called before the
// config watcher starts, so no reload can race with registration.
func SetReloadHook(hook ReloadHook) {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	reloadHook = hook
}

// ApplyReload invokes the registered reload hook.
func ApplyReload(ctx context.Context, next *Snapshot) error {
	reloadMu.RLock()
	hook := reloadHook
	reloadMu.RUnlock()
	if hook == nil {
		return gerror.New("config reload hook is not registered")
	}
	return hook(ctx, next)
}

// Config is the top-level config structure (manifest/config/config.yaml).
type Config struct {
	App    AppConfig    `json:"app"    yaml:"app"`
	Logger LoggerConfig `json:"logger" yaml:"logger"`
	Server ServerConfig `json:"server" yaml:"server"`
	Chain  ChainConfig  `json:"chain"  yaml:"chain"`
}

// AppConfig is the application metadata.
type AppConfig struct {
	Name    string `json:"name"    yaml:"name"`
	Version string `json:"version" yaml:"version"`
}

// LoggerConfig is the logging config.
type LoggerConfig struct {
	// Level is one of debug / info / warn / error.
	Level string `json:"level" yaml:"level"`
}

// TLSConfig enables TLS or mTLS on the ext_proc gRPC listener. It is optional:
// sidecar deployments normally run plaintext on loopback, while cross-pod
// deployments should enable TLS (and usually mTLS via ClientCAFile).
type TLSConfig struct {
	Enabled      bool   `json:"enabled"      yaml:"enabled"`
	CertFile     string `json:"certFile"     yaml:"certFile"`
	KeyFile      string `json:"keyFile"      yaml:"keyFile"`
	ClientCAFile string `json:"clientCAFile" yaml:"clientCAFile"`
	// MinVersion is "1.2" or "1.3"; empty defaults to "1.2".
	MinVersion string `json:"minVersion" yaml:"minVersion"`
	// ReloadIntervalSeconds polls the certificate/CA files for rotation. Zero
	// disables polling, so rotation requires a restart.
	ReloadIntervalSeconds int `json:"reloadIntervalSeconds" yaml:"reloadIntervalSeconds"`
}

// ServerConfig is the ext_proc service config.
type ServerConfig struct {
	// BindAddress is the gRPC listen address. Use 127.0.0.1 for a same-pod
	// sidecar and 0.0.0.0 (or a specific pod IP) for a cross-pod deployment.
	BindAddress string `json:"bindAddress" yaml:"bindAddress"`
	// MetricsBindAddress is the Prometheus listener address. Keep it on
	// loopback for sidecars; expose it only to the monitoring namespace for
	// cross-pod deployments.
	MetricsBindAddress string `json:"metricsBindAddress" yaml:"metricsBindAddress"`
	// ExtProcPort is the ext_proc gRPC listen port (Envoy's grpc_service
	// points here).
	ExtProcPort int `json:"extProcPort" yaml:"extProcPort"`
	// MetricsPort is the Prometheus metrics port.
	MetricsPort int `json:"metricsPort" yaml:"metricsPort"`
	// MaxConcurrentStreams is the gRPC per-process concurrent-stream cap (one
	// goroutine per stream).
	MaxConcurrentStreams uint32 `json:"maxConcurrentStreams" yaml:"maxConcurrentStreams"`
	// MaxRecvMsgSize / MaxSendMsgSize are gRPC message limits in bytes. They
	// must be at least as large as the bodies Envoy is configured to buffer.
	MaxRecvMsgSize int `json:"maxRecvMsgSize" yaml:"maxRecvMsgSize"`
	MaxSendMsgSize int `json:"maxSendMsgSize" yaml:"maxSendMsgSize"`
	// TLS is the gRPC listener security configuration.
	TLS TLSConfig `json:"tls" yaml:"tls"`
	// UnreadyPropagationDelaySeconds is the drain delay between marking
	// unready and stopping the server.
	UnreadyPropagationDelaySeconds uint32 `json:"unreadyPropagationDelaySeconds" yaml:"unreadyPropagationDelaySeconds"`
	// TerminationGracePeriodSeconds bounds the in-flight stream drain; past
	// it the process force-exits.
	TerminationGracePeriodSeconds int32 `json:"terminationGracePeriodSeconds" yaml:"terminationGracePeriodSeconds"`
}

// ChainConfig is the chain orchestration config (type alias to
// plugin.ChainConfig). Plugin-private configs are not typed; they are read
// from Snapshot.Raw by BuildRouteTable.
type ChainConfig = plugin.ChainConfig

// ApplyDefaults fills optional fields with safe defaults. It never overrides
// values that are explicitly set.
func (c *Config) ApplyDefaults() {
	if c.App.Name == "" {
		c.App.Name = "ext-proc"
	}
	if c.Logger.Level == "" {
		c.Logger.Level = "info"
	}
	if c.Server.BindAddress == "" {
		c.Server.BindAddress = "0.0.0.0"
	}
	if c.Server.MetricsBindAddress == "" {
		c.Server.MetricsBindAddress = "0.0.0.0"
	}
	if c.Server.MaxConcurrentStreams == 0 {
		c.Server.MaxConcurrentStreams = 1000
	}
	if c.Server.MaxRecvMsgSize == 0 {
		c.Server.MaxRecvMsgSize = 4 << 20
	}
	if c.Server.MaxSendMsgSize == 0 {
		c.Server.MaxSendMsgSize = 4 << 20
	}
	if c.Server.TLS.MinVersion == "" {
		c.Server.TLS.MinVersion = "1.2"
	}
	if c.Chain.FailurePolicy == "" {
		c.Chain.FailurePolicy = "fail_closed"
	}
}

// Validate checks the whole config and returns a startup error for anything
// that would otherwise fail silently at request time.
func (c Config) Validate() error {
	if c.App.Name == "" {
		return gerror.New("app.name must not be empty")
	}
	switch strings.ToLower(c.Logger.Level) {
	case "debug", "info", "warn", "error":
	default:
		return gerror.Newf("logger.level %q is invalid (want debug/info/warn/error)", c.Logger.Level)
	}
	if err := c.Server.Validate(); err != nil {
		return err
	}
	switch c.Chain.FailurePolicy {
	case "", "fail_open", "fail_closed":
	default:
		return gerror.Newf("chain.failurePolicy %q is invalid (want fail_open/fail_closed)", c.Chain.FailurePolicy)
	}
	for i, pc := range c.Chain.Plugins {
		if pc.Name == "" {
			return gerror.Newf("chain.plugins[%d].name must not be empty", i)
		}
		if pc.Order < 0 {
			return gerror.Newf("chain.plugins[%d] (%s): order must not be negative", i, pc.Name)
		}
		switch pc.FailurePolicy {
		case "", "fail_open", "fail_closed":
		default:
			return gerror.Newf("chain.plugins[%d] (%s): failurePolicy %q is invalid", i, pc.Name, pc.FailurePolicy)
		}
		for j, rule := range pc.MatchRules {
			if rule.PathPrefix != "" && !strings.HasPrefix(rule.PathPrefix, "/") {
				return gerror.Newf("chain.plugins[%d] (%s).matchRules[%d]: pathPrefix must start with /", i, pc.Name, j)
			}
			for k, domain := range rule.Domain {
				if strings.TrimSpace(domain) == "" {
					return gerror.Newf("chain.plugins[%d] (%s).matchRules[%d].domain[%d] must not be empty", i, pc.Name, j, k)
				}
			}
		}
	}
	return nil
}

// Validate checks the server section.
func (s ServerConfig) Validate() error {
	if err := validatePort("server.extProcPort", s.ExtProcPort); err != nil {
		return err
	}
	if err := validatePort("server.metricsPort", s.MetricsPort); err != nil {
		return err
	}
	if s.BindAddress == s.MetricsBindAddress && s.ExtProcPort == s.MetricsPort {
		return gerror.New("server.extProcPort and server.metricsPort must differ when bind addresses are the same")
	}
	if s.MaxConcurrentStreams == 0 {
		return gerror.New("server.maxConcurrentStreams must be greater than zero")
	}
	if s.MaxRecvMsgSize <= 0 || s.MaxSendMsgSize <= 0 {
		return gerror.New("server.maxRecvMsgSize and server.maxSendMsgSize must be greater than zero")
	}
	if s.TerminationGracePeriodSeconds < 0 {
		return gerror.New("server.terminationGracePeriodSeconds must not be negative")
	}
	switch s.TLS.MinVersion {
	case "1.2", "1.3":
	default:
		return gerror.Newf("server.tls.minVersion %q is invalid (want 1.2 or 1.3)", s.TLS.MinVersion)
	}
	if s.TLS.ReloadIntervalSeconds < 0 {
		return gerror.New("server.tls.reloadIntervalSeconds must not be negative")
	}
	if s.TLS.Enabled {
		if s.TLS.CertFile == "" || s.TLS.KeyFile == "" {
			return gerror.New("server.tls.certFile and server.tls.keyFile are required when TLS is enabled")
		}
	} else if s.TLS.CertFile != "" || s.TLS.KeyFile != "" || s.TLS.ClientCAFile != "" {
		return gerror.New("server.tls files are set but server.tls.enabled is false")
	}
	return nil
}

func validatePort(field string, port int) error {
	if port < 1 || port > 65535 {
		return gerror.Newf("%s %d is invalid (must be 1-65535)", field, port)
	}
	return nil
}

// ValidateReloadable rejects changes to sections that are captured once at
// startup. Only the chain (and plugin config) is hot-reloadable.
func ValidateReloadable(old, next Config) error {
	if !reflect.DeepEqual(old.App, next.App) {
		return gerror.New("app section cannot be hot-reloaded; restart the process to apply it")
	}
	if !reflect.DeepEqual(old.Logger, next.Logger) {
		return gerror.New("logger section cannot be hot-reloaded; restart the process to apply it")
	}
	if !reflect.DeepEqual(old.Server, next.Server) {
		return gerror.New("server section cannot be hot-reloaded; restart the process to apply it")
	}
	return nil
}
