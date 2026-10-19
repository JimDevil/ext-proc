package config

import (
	"testing"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

func validConfig() Config {
	c := Config{
		App:    AppConfig{Name: "ext-proc", Version: "test"},
		Logger: LoggerConfig{Level: "info"},
		Server: ServerConfig{
			BindAddress:                    "127.0.0.1",
			MetricsBindAddress:             "127.0.0.1",
			ExtProcPort:                    50051,
			MetricsPort:                    19090,
			MaxConcurrentStreams:           1000,
			MaxRecvMsgSize:                 4 << 20,
			MaxSendMsgSize:                 4 << 20,
			UnreadyPropagationDelaySeconds: 5,
			TerminationGracePeriodSeconds:  15,
		},
		Chain: plugin.ChainConfig{FailurePolicy: "fail_closed"},
	}
	c.ApplyDefaults()
	return c
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	t.Parallel()
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"invalid log level", func(c *Config) { c.Logger.Level = "verbose" }},
		{"zero extproc port", func(c *Config) { c.Server.ExtProcPort = 0 }},
		{"same ports", func(c *Config) { c.Server.MetricsPort = c.Server.ExtProcPort }},
		{"zero concurrent streams", func(c *Config) { c.Server.MaxConcurrentStreams = 0 }},
		{"zero message size", func(c *Config) { c.Server.MaxRecvMsgSize = 0 }},
		{"tls missing cert", func(c *Config) {
			c.Server.TLS = TLSConfig{Enabled: true, KeyFile: "key.pem", MinVersion: "1.2"}
		}},
		{"tls files without enabled", func(c *Config) {
			c.Server.TLS = TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.2"}
		}},
		{"invalid tls version", func(c *Config) {
			c.Server.TLS = TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.1"}
		}},
		{"negative tls reload", func(c *Config) {
			c.Server.TLS = TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem", MinVersion: "1.2", ReloadIntervalSeconds: -1}
		}},
		{"invalid chain policy", func(c *Config) { c.Chain.FailurePolicy = "sometimes" }},
		{"empty plugin name", func(c *Config) {
			c.Chain.Plugins = []plugin.PluginConfig{{Order: 10}}
		}},
		{"negative order", func(c *Config) {
			c.Chain.Plugins = []plugin.PluginConfig{{Name: "p", Order: -1}}
		}},
		{"invalid plugin policy", func(c *Config) {
			c.Chain.Plugins = []plugin.PluginConfig{{Name: "p", FailurePolicy: "nope"}}
		}},
		{"invalid path prefix", func(c *Config) {
			c.Chain.Plugins = []plugin.PluginConfig{{Name: "p", MatchRules: []plugin.MatchRule{{PathPrefix: "v1"}}}}
		}},
		{"empty domain", func(c *Config) {
			c.Chain.Plugins = []plugin.PluginConfig{{Name: "p", MatchRules: []plugin.MatchRule{{Domain: []string{" "}}}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}

func TestValidateReloadable(t *testing.T) {
	t.Parallel()

	old := validConfig()
	next := validConfig()
	next.Chain.Plugins = []plugin.PluginConfig{{Name: "p"}}
	if err := ValidateReloadable(old, next); err != nil {
		t.Fatalf("chain changes must be reloadable: %v", err)
	}

	next = validConfig()
	next.Server.ExtProcPort = 50052
	if err := ValidateReloadable(old, next); err == nil {
		t.Fatal("server changes must require a restart")
	}

	next = validConfig()
	next.Logger.Level = "debug"
	if err := ValidateReloadable(old, next); err == nil {
		t.Fatal("logger changes must require a restart")
	}
}

func TestSnapshotStoreAndLogger(t *testing.T) {
	old := Current()
	t.Cleanup(func() { Store(old) })

	Store(&Snapshot{Cfg: validConfig()})
	if Current().Cfg.App.Name != "ext-proc" {
		t.Fatal("snapshot was not published")
	}
	// ApplyDefaults is idempotent and the reload validation must accept the
	// same generation.
	next := Current().Cfg
	next.ApplyDefaults()
	if err := ValidateReloadable(Current().Cfg, next); err != nil {
		t.Fatalf("identical generation must be reloadable: %v", err)
	}
}
