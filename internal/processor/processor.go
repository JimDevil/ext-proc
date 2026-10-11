// Package processor bridges the SDK's six phase callbacks to the plugin
// responsibility chain. Its only job is translation between the two semantics
// (RequestContext <-> plugin.Context); all business logic lives in
// internal/plugins.
package processor

import (
	"log/slog"
	"sync"
	"sync/atomic"

	extproc "github.com/JimDevil/ext-proc/sdk"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

var global atomic.Pointer[ChainProcessor]

// Register stores the global processor (set by boot after assembly; read by
// server.Run).
func Register(p *ChainProcessor) { global.Store(p) }

// Global returns the global processor; nil before initialization.
func Global() *ChainProcessor { return global.Load() }

// ChainProcessor implements the SDK's RequestProcessor and holds the current
// route table (global chain + matchRule variants). SwapChain atomically
// replaces the table on hot reload; in-flight streams keep using the old table
// until they finish.
type ChainProcessor struct {
	name      string
	opts      *extproc.ProcessingOptions
	logger    *slog.Logger
	closeOnce sync.Once

	mu    sync.RWMutex
	table *plugin.RouteTable
}

// New builds the bridge. opts forces AbortOnProcessorFailure=true: whether a
// plugin failure aborts the request is decided by the chain's failurePolicy
// (fail_open is absorbed inside the chain), while the SDK layer keeps
// fail-closed as the last line of defense — otherwise "chain configured as
// fail_closed but SDK swallows the error" would silently break the semantics.
func New(name string, logger *slog.Logger) *ChainProcessor {
	opts := extproc.NewDefaultProcessingOptions()
	opts.AbortOnProcessorFailure = true
	opts.UpdateDurationHeader = false
	return &ChainProcessor{name: name, opts: opts, logger: logger}
}

// SwapChain atomically replaces the route table (hot-reload entry point). The
// old table is NOT closed (see the plugin.Plugin.Close contract: plugins must
// not hold resources that need explicit release).
func (p *ChainProcessor) SwapChain(t *plugin.RouteTable) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.table = t
}

// current returns the active route table.
func (p *ChainProcessor) current() *plugin.RouteTable {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table
}

// chainStateKey is the reserved request-state key under which the resolved
// chain variant is stored during the request-headers phase (the later phases
// cannot see headers, so the decision must be stashed on the stream).
const chainStateKey = "extproc.chain"

// chainFor returns the chain variant resolved for this stream. Streams that
// never went through a request-headers phase (e.g. an earlier filter already
// replied) fall back to the global composition.
func (p *ChainProcessor) chainFor(rc *extproc.RequestContext) *plugin.Chain {
	if v, err := rc.GetValue(chainStateKey); err == nil {
		if c, ok := v.(*plugin.Chain); ok {
			return c
		}
	}
	return p.current().GlobalChain()
}

// GetName implements extproc.RequestProcessor.
func (p *ChainProcessor) GetName() string { return p.name }

// GetOptions implements extproc.RequestProcessor.
func (p *ChainProcessor) GetOptions() *extproc.ProcessingOptions { return p.opts }

// ProcessRequestHeaders implements extproc.RequestProcessor: composes the
// per-request chain (each plugin resolves its own matchRules by
// authority/path), stashes the composition for the later phases, and forwards
// the request-headers phase to it.
func (p *ChainProcessor) ProcessRequestHeaders(rc *extproc.RequestContext, headers extproc.AllHeaders) error {
	chain := p.current().Compose(rc.Authority, rc.Path)
	_ = rc.SetValue(chainStateKey, chain)
	return chain.RunRequestHeaders(newStreamContext(rc), headers)
}

// ProcessRequestBody implements extproc.RequestProcessor: forwards the
// request-body phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessRequestBody(rc *extproc.RequestContext, body []byte) error {
	return p.chainFor(rc).RunRequestBody(newStreamContext(rc), body)
}

// ProcessRequestTrailers implements extproc.RequestProcessor: forwards the
// request-trailers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessRequestTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	return p.chainFor(rc).RunRequestTrailers(newStreamContext(rc), trailers)
}

// ProcessResponseHeaders implements extproc.RequestProcessor: forwards the
// response-headers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseHeaders(rc *extproc.RequestContext, headers extproc.AllHeaders) error {
	return p.chainFor(rc).RunResponseHeaders(newStreamContext(rc), headers)
}

// ProcessResponseBody implements extproc.RequestProcessor: forwards the
// response-body phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseBody(rc *extproc.RequestContext, body []byte) error {
	return p.chainFor(rc).RunResponseBody(newStreamContext(rc), body)
}

// ProcessResponseTrailers implements extproc.RequestProcessor: forwards the
// response-trailers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	return p.chainFor(rc).RunResponseTrailers(newStreamContext(rc), trailers)
}

// ErrorHandler implements extproc.RequestProcessor: SDK-level errors
// (including chain fail_closed escalations) are logged here; the fate of the
// stream is decided by the SDK based on AbortOnProcessorFailure.
func (p *ChainProcessor) ErrorHandler(rc *extproc.RequestContext, phase int, err error) {
	p.logger.Error("ext_proc phase processing failed",
		slog.String("phase", extproc.RequestPhaseToString(phase)),
		slog.String("error", err.Error()))
}

// Close implements extproc.RequestProcessor: closes every plugin instance of
// the live generation (global chain + all matchRule variants) during graceful
// shutdown.
func (p *ChainProcessor) Close(gracePeriodSeconds int32) error {
	p.closeOnce.Do(func() {
		if t := p.current(); t != nil {
			t.CloseAll()
		}
	})
	return nil
}

// streamContext adapts the SDK's RequestContext to the plugin.Context narrow
// interface. The aborted flag is the single source of truth for
// short-circuiting: plugins can only set an immediate response through this
// interface, so flagging it in the bridge is accurate.
type streamContext struct {
	rc      *extproc.RequestContext
	aborted bool
}

// newStreamContext builds a fresh adapter per stream message. aborted does not
// leak across phases — Envoy sends no further messages after an
// ImmediateResponse anyway.
func newStreamContext(rc *extproc.RequestContext) plugin.Context {
	return &streamContext{rc: rc}
}

func (s *streamContext) ContinueRequest() error { return s.rc.ContinueRequest() }

func (s *streamContext) CancelRequest(status int32, headers map[string]extproc.HeaderValue, body string) error {
	s.aborted = true
	return s.rc.CancelRequest(status, headers, body)
}

func (s *streamContext) AppendHeader(name string, hv extproc.HeaderValue) error {
	return s.rc.AppendHeader(name, hv)
}

func (s *streamContext) OverwriteHeader(name string, hv extproc.HeaderValue) error {
	return s.rc.OverwriteHeader(name, hv)
}

func (s *streamContext) RemoveHeader(name string) error { return s.rc.RemoveHeader(name) }

func (s *streamContext) ReplaceBody(body []byte) error { return s.rc.ReplaceBody(body) }

func (s *streamContext) EndOfStream() bool { return s.rc.EndOfStream }

func (s *streamContext) SetValue(name string, val any) error { return s.rc.SetValue(name, val) }

func (s *streamContext) GetValue(name string) (any, bool) {
	v, err := s.rc.GetValue(name)
	if err != nil {
		return nil, false
	}
	return v, true
}

func (s *streamContext) Aborted() bool { return s.aborted }
