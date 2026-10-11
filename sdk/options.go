package extproc

import (
	"encoding/json"
	"os"

	yaml "gopkg.in/yaml.v3"
)

// ServerOptions are the service-level options (gRPC port / metrics port /
// concurrency and graceful-shutdown parameters).
type ServerOptions struct {
	ExtProcPort                    int    `json:"extproc_port" yaml:"extproc_port"`
	MetricsHTTPPort                int    `json:"metrics_http_port" yaml:"metrics_http_port"`
	MaxConcurrentStreams           uint32 `json:"max_concurrent_streams" yaml:"max_concurrent_streams"`
	UnreadyPropagationDelaySeconds uint32 `json:"unready_propagation_delay_seconds" yaml:"unready_propagation_delay_seconds"`
	TerminationGracePeriodSeconds  int32  `json:"termination_grace_period_seconds" yaml:"termination_grace_period_seconds"`
}

func NewDefaultServerOptions() *ServerOptions {
	return &ServerOptions{
		ExtProcPort:                    50051,
		MetricsHTTPPort:                9090,
		MaxConcurrentStreams:           1000,
		UnreadyPropagationDelaySeconds: 5,
		TerminationGracePeriodSeconds:  10,
	}
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
	return &ProcessingOptions{}
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
