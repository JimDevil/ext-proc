package pluginhost

import (
	"log/slog"

	"github.com/JimDevil/ext-proc/internal/plugin"
	"github.com/JimDevil/ext-proc/internal/processor"
	extproc "github.com/JimDevil/ext-proc/sdk"
)

// singleProcessor bridges ONE in-process plugin.Plugin to the ext_proc wire
// protocol — the subprocess side of a dynamic plugin. It is the same
// translation the main service's ChainProcessor does, minus the chain: the
// host process owns orchestration (order, matchRules, failurePolicy), this
// process only runs the plugin. Mutations the plugin makes on its Context
// accumulate in the RequestContext and flow back to the host as the phase's
// CommonResponse / ImmediateResponse.
type singleProcessor struct {
	name   string
	p      plugin.Plugin
	opts   *extproc.ProcessingOptions
	logger *slog.Logger
}

// NewSingleProcessor wraps one in-process plugin into the SDK's
// RequestProcessor contract. Exported for the dynamic plugin tests and for
// anyone embedding a single plugin into their own ext_proc server; the
// subprocess binary path uses newSingleProcessor via Serve.
func NewSingleProcessor(name string, p plugin.Plugin, logger *slog.Logger) extproc.RequestProcessor {
	return newSingleProcessor(name, p, logger)
}

func newSingleProcessor(name string, p plugin.Plugin, logger *slog.Logger) *singleProcessor {
	opts := extproc.NewDefaultProcessingOptions()
	opts.AbortOnProcessorFailure = true
	opts.UpdateDurationHeader = false
	return &singleProcessor{name: name, p: p, opts: opts, logger: logger}
}

func (s *singleProcessor) GetName() string                        { return s.name }
func (s *singleProcessor) GetOptions() *extproc.ProcessingOptions { return s.opts }

func (s *singleProcessor) ProcessRequestHeaders(rc *extproc.RequestContext, headers extproc.AllHeaders) error {
	return s.p.OnRequestHeaders(processor.NewStreamContext(rc), headers)
}

func (s *singleProcessor) ProcessRequestBody(rc *extproc.RequestContext, body []byte) error {
	return s.p.OnRequestBody(processor.NewStreamContext(rc), body)
}

func (s *singleProcessor) ProcessRequestTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	return s.p.OnRequestTrailers(processor.NewStreamContext(rc), trailers)
}

func (s *singleProcessor) ProcessResponseHeaders(rc *extproc.RequestContext, headers extproc.AllHeaders) error {
	return s.p.OnResponseHeaders(processor.NewStreamContext(rc), headers)
}

func (s *singleProcessor) ProcessResponseBody(rc *extproc.RequestContext, body []byte) error {
	return s.p.OnResponseBody(processor.NewStreamContext(rc), body)
}

func (s *singleProcessor) ProcessResponseTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	return s.p.OnResponseTrailers(processor.NewStreamContext(rc), trailers)
}

// ErrorHandler logs subprocess-level phase errors; the fate of the stream is
// decided by the SDK (AbortOnProcessorFailure=true keeps fail-closed as the
// last line of defense — the host applies its own failurePolicy on top).
func (s *singleProcessor) ErrorHandler(rc *extproc.RequestContext, phase int, err error) {
	s.logger.Error("plugin phase processing failed",
		slog.String("phase", extproc.RequestPhaseToString(phase)),
		slog.String("plugin", s.name),
		slog.String("error", err.Error()))
}

// Close releases the plugin instance when the process drains.
func (s *singleProcessor) Close(gracePeriodSeconds int32) error {
	return s.p.Close()
}
