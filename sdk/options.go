package extproc

import (
	"encoding/json"
	"fmt"
	"os"

	yaml "gopkg.in/yaml.v3"
)

// ServerOptions are the service-level options (gRPC port / metrics port /
// concurrency and graceful-shutdown parameters).
type ServerOptions struct {
	// BindAddress / MetricsBindAddress select the listen interfaces. Empty
	// means all interfaces. A sidecar deployment should use 127.0.0.1; a
	// cross-pod deployment uses 0.0.0.0 behind a NetworkPolicy/TLS.
	BindAddress                    string      `json:"bind_address" yaml:"bind_address"`
	MetricsBindAddress             string      `json:"metrics_bind_address" yaml:"metrics_bind_address"`
	ExtProcPort                    int         `json:"extproc_port" yaml:"extproc_port"`
	MetricsHTTPPort                int         `json:"metrics_http_port" yaml:"metrics_http_port"`
	MaxConcurrentStreams           uint32      `json:"max_concurrent_streams" yaml:"max_concurrent_streams"`
	MaxRecvMsgSize                 int         `json:"max_recv_msg_size" yaml:"max_recv_msg_size"`
	MaxSendMsgSize                 int         `json:"max_send_msg_size" yaml:"max_send_msg_size"`
	TLS                            *TLSOptions `json:"tls" yaml:"tls"`
	UnreadyPropagationDelaySeconds uint32      `json:"unready_propagation_delay_seconds" yaml:"unready_propagation_delay_seconds"`
	TerminationGracePeriodSeconds  int32       `json:"termination_grace_period_seconds" yaml:"termination_grace_period_seconds"`
}

// TLSOptions configures TLS/mTLS on the ext_proc gRPC listener. ClientCAFile
// enables mutual TLS: when set, clients (Envoy) must present a certificate
// signed by this CA.
type TLSOptions struct {
	CertFile     string `json:"cert_file" yaml:"cert_file"`
	KeyFile      string `json:"key_file" yaml:"key_file"`
	ClientCAFile string `json:"client_ca_file" yaml:"client_ca_file"`
	// MinVersion is "1.2" or "1.3"; empty defaults to "1.2".
	MinVersion string `json:"min_version" yaml:"min_version"`
	// ReloadIntervalSeconds enables certificate/CA polling. Zero disables it;
	// a failed reload keeps the previous material.
	ReloadIntervalSeconds int `json:"reload_interval_seconds" yaml:"reload_interval_seconds"`
}

func NewDefaultServerOptions() *ServerOptions {
	return &ServerOptions{
		ExtProcPort:                    50051,
		MetricsHTTPPort:                9090,
		MaxConcurrentStreams:           1000,
		MaxRecvMsgSize:                 4 << 20,
		MaxSendMsgSize:                 4 << 20,
		UnreadyPropagationDelaySeconds: 5,
		TerminationGracePeriodSeconds:  10,
	}
}

// Validate rejects a server configuration that would fail at listen time or
// silently downgrade security.
func (o *ServerOptions) Validate() error {
	if o == nil {
		return fmt.Errorf("server options are nil")
	}
	if err := validatePort("extproc port", o.ExtProcPort); err != nil {
		return err
	}
	if err := validatePort("metrics port", o.MetricsHTTPPort); err != nil {
		return err
	}
	if o.BindAddress == o.MetricsBindAddress && o.ExtProcPort == o.MetricsHTTPPort {
		return fmt.Errorf("extproc and metrics ports must differ when bind addresses are the same")
	}
	if o.MaxConcurrentStreams == 0 {
		return fmt.Errorf("max concurrent streams must be greater than zero")
	}
	if o.MaxRecvMsgSize <= 0 || o.MaxSendMsgSize <= 0 {
		return fmt.Errorf("max receive/send message sizes must be greater than zero")
	}
	if o.TerminationGracePeriodSeconds < 0 {
		return fmt.Errorf("termination grace period must not be negative")
	}
	if o.TLS != nil {
		if o.TLS.CertFile == "" || o.TLS.KeyFile == "" {
			return fmt.Errorf("TLS cert_file and key_file are required")
		}
		switch o.TLS.MinVersion {
		case "", "1.2", "1.3":
		default:
			return fmt.Errorf("invalid TLS min_version %q (want 1.2 or 1.3)", o.TLS.MinVersion)
		}
		if o.TLS.ReloadIntervalSeconds < 0 {
			return fmt.Errorf("TLS reload_interval_seconds must not be negative")
		}
	}
	return nil
}

func validatePort(field string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d is invalid (must be 1-65535)", field, port)
	}
	return nil
}

func NewServerOptionsFromJson(filePath string) (*ServerOptions, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var opts ServerOptions
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&opts); err != nil {
		return nil, err
	}

	return &opts, nil
}

func NewServerOptionsFromYaml(filePath string) (*ServerOptions, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var opts ServerOptions
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&opts); err != nil {
		return nil, err
	}

	return &opts, nil
}

// ProcessingOptions are the processing-level behavior switches.
type ProcessingOptions struct {
	// UpdateExtProcHeader appends x-extproc-names in the response-headers phase
	// (marks the processing chain) — debugging aid.
	UpdateExtProcHeader bool
	// UpdateDurationHeader injects x-extproc-duration-ns at end of stream —
	// debugging aid.
	UpdateDurationHeader bool
	// AbortOnProcessorFailure aborts the stream when the processor errors
	// (true = fail-closed); false swallows the error and returns an empty
	// continue (fail-open).
	AbortOnProcessorFailure bool
}

func NewDefaultProcessingOptions() *ProcessingOptions {
	// Fail closed by default: a processor error aborts the stream. Callers
	// that explicitly want fail-open must opt in.
	return &ProcessingOptions{AbortOnProcessorFailure: true}
}

func NewProcessingOptionsFromJson(filePath string) (*ProcessingOptions, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var opts ProcessingOptions
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&opts); err != nil {
		return nil, err
	}

	return &opts, nil
}

func NewProcessingOptionsFromYaml(filePath string) (*ProcessingOptions, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var opts ProcessingOptions
	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&opts); err != nil {
		return nil, err
	}

	return &opts, nil
}
