// Package config defines the global config structs and holds runtime global
// state (config snapshot / raw tree / logger). Pattern: boot finishes loading
// and writes back to package-level variables.
package config

import (
	"context"
	"log/slog"

	"github.com/gogf/gf/v2/encoding/gjson"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

var (
	// Cfg is the active typed config, replaced whole on hot reload.
	Cfg Config
	// Raw is the full config tree (JSON view); each plugin's private config
	// section (chain.plugins[].config) is extracted from here.
	Raw *gjson.Json
	// Log is the global structured logger, initialized by boot.InitLog.
	Log *slog.Logger
	// OnConfigReload is the hot-reload hook, registered by the chain assembly
	// (boot.InitChain) and invoked by the gcfg watcher to rebuild the chain.
	OnConfigReload func(ctx context.Context) error
)

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
	// Level is one of debug / info / warn / error, defaulting to info.
	Level string `json:"level" yaml:"level"`
}

// ServerConfig is the ext_proc service config.
type ServerConfig struct {
	// ExtProcPort is the ext_proc gRPC listen port (Envoy's grpc_service
	// points here).
	ExtProcPort int `json:"extProcPort" yaml:"extProcPort"`
	// MetricsPort is the Prometheus metrics port.
	MetricsPort int `json:"metricsPort" yaml:"metricsPort"`
	// MaxConcurrentStreams is the gRPC per-process concurrent-stream cap (one
	// goroutine per stream).
	MaxConcurrentStreams uint32 `json:"maxConcurrentStreams" yaml:"maxConcurrentStreams"`
	// UnreadyPropagationDelaySeconds is the drain delay between marking
	// unready and stopping the server.
	UnreadyPropagationDelaySeconds uint32 `json:"unreadyPropagationDelaySeconds" yaml:"unreadyPropagationDelaySeconds"`
	// TerminationGracePeriodSeconds bounds the in-flight stream drain; past
	// it the process force-exits.
	TerminationGracePeriodSeconds int32 `json:"terminationGracePeriodSeconds" yaml:"terminationGracePeriodSeconds"`
}

// ChainConfig is the chain orchestration config (type alias to
// plugin.ChainConfig). Plugin-private configs are not typed; BuildChain
// extracts them from config.Raw by plugin name.
type ChainConfig = plugin.ChainConfig
