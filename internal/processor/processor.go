// Package processor bridges the SDK's six phase callbacks to the plugin
// filter chain. Its only job is translation between the two semantics
// (RequestContext <-> plugin.Context); all business logic lives in
// internal/plugins.
package processor

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/errors/gerror"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

var global atomic.Pointer[ChainProcessor]

var _ extproc.StreamLifecycle = (*ChainProcessor)(nil)

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

	mu       sync.RWMutex
	table    *tableRef
	observer *MetricsObserver
}

// tableRef owns one route-table generation and tracks the streams currently
// using it. A generation is retired on SwapChain and closed exactly once when
// its last stream ends, so hot reload never closes plugin instances that an
// in-flight stream still uses and never leaks the old generation.
type tableRef struct {
	table     *plugin.RouteTable
	refs      atomic.Int64
	retired   atomic.Bool
	closeOnce sync.Once
}

func newTableRef(table *plugin.RouteTable) *tableRef {
	return &tableRef{table: table}
}

func (r *tableRef) acquire() { r.refs.Add(1) }

func (r *tableRef) release() {
	if r.refs.Add(-1) == 0 && r.retired.Load() {
		r.close()
	}
}

func (r *tableRef) retire() {
	r.retired.Store(true)
	if r.refs.Load() == 0 {
		r.close()
	}
}

func (r *tableRef) close() {
	r.closeOnce.Do(func() {
		if r.table != nil {
			r.table.CloseAll()
		}
	})
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
	return &ChainProcessor{name: name, opts: opts, logger: logger, observer: &MetricsObserver{}}
}

// Observer exposes the chain metrics observer for BuildRouteTable.
func (p *ChainProcessor) Observer() plugin.Observer { return p.observer }

// SetMetrics installs the Prometheus metric set used by plugin-level metrics.
func (p *ChainProcessor) SetMetrics(metrics *extproc.Metrics) {
	if p.observer != nil {
		p.observer.SetMetrics(metrics)
	}
}

// SwapChain atomically replaces the route table (hot-reload entry point). The
// old generation is retired and closed as soon as its last in-flight stream
// finishes; in-flight streams keep using the old plugin instances.
func (p *ChainProcessor) SwapChain(t *plugin.RouteTable) {
	next := newTableRef(t)
	p.mu.Lock()
	old := p.table
	p.table = next
	p.mu.Unlock()
	if old != nil {
		old.retire()
	}
}

// current returns the active generation.
func (p *ChainProcessor) current() *tableRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table
}

// acquireCurrent returns a pinned reference to the active generation. The
// mutex prevents acquiring a generation that SwapChain retired and closed
// between the load and the counter increment.
func (p *ChainProcessor) acquireCurrent() *tableRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref := p.table
	if ref != nil {
		ref.acquire()
	}
	return ref
}

// streamTableKey stores the pinned generation for the lifetime of the stream.
const streamTableKey = "extproc.table"

// streamRef returns the stream's pinned generation, or nil when the SDK did
// not call StreamStart (direct/test usage).
func (p *ChainProcessor) streamRef(rc *extproc.RequestContext) *tableRef {
	if v, err := rc.GetValue(streamTableKey); err == nil {
		if ref, ok := v.(*tableRef); ok {
			return ref
		}
	}
	return nil
}

// StreamStart pins the active generation for the whole stream. It implements
// extproc.StreamLifecycle.
func (p *ChainProcessor) StreamStart(rc *extproc.RequestContext) {
	if p.streamRef(rc) != nil {
		return
	}
	ref := p.acquireCurrent()
	if ref == nil {
		return
	}
	if err := rc.SetValue(streamTableKey, ref); err != nil {
		p.logger.Error("failed to pin the plugin generation", slog.String("error", err.Error()))
		ref.release()
	}
}

// StreamEnd releases the stream's pinned generation. The old generation is
// closed here when it was retired by a hot reload.
func (p *ChainProcessor) StreamEnd(rc *extproc.RequestContext) {
	ref := p.streamRef(rc)
	if ref == nil {
		return
	}
	// Clear the slot first so a duplicated StreamEnd cannot release twice.
	if err := rc.SetValue(streamTableKey, (*tableRef)(nil)); err != nil {
		p.logger.Error("failed to clear the plugin generation pin", slog.String("error", err.Error()))
	}
	ref.release()
}

// chainStateKey is the reserved request-state key under which the resolved
// chain variant is stored during the request-headers phase (the later phases
// cannot see headers, so the decision must be stashed on the stream).
const chainStateKey = "extproc.chain"

// chainFor returns the chain variant resolved for this stream. Streams that
// never went through a request-headers phase (e.g. an earlier filter already
// replied) fall back to the global composition.
func (p *ChainProcessor) chainFor(rc *extproc.RequestContext) (*plugin.Chain, error) {
	if v, err := rc.GetValue(chainStateKey); err == nil {
		if c, ok := v.(*plugin.Chain); ok {
			return c, nil
		}
	}
	ref := p.streamRef(rc)
	if ref == nil {
		// The SDK lifecycle is optional; direct callers may start at a body
		// phase. Pin the current generation so it cannot be closed mid-phase.
		ref = p.acquireCurrent()
		if ref == nil {
			return nil, gerror.New("plugin route table is not initialized")
		}
		if err := rc.SetValue(streamTableKey, ref); err != nil {
			ref.release()
			return nil, gerror.Wrap(err, "failed to pin the plugin generation")
		}
	}
	return ref.table.GlobalChain(), nil
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
	ref := p.streamRef(rc)
	if ref == nil {
		ref = p.acquireCurrent()
		if ref == nil {
			return gerror.New("plugin route table is not initialized")
		}
		if err := rc.SetValue(streamTableKey, ref); err != nil {
			ref.release()
			return gerror.Wrap(err, "failed to pin the plugin generation")
		}
	}
	chain := ref.table.Compose(rc.Authority, rc.Path)
	if err := rc.SetValue(chainStateKey, chain); err != nil {
		return gerror.Wrap(err, "failed to store the resolved chain")
	}
	return chain.RunRequestHeaders(newStreamContext(rc), headers)
}

// ProcessRequestBody implements extproc.RequestProcessor: forwards the
// request-body phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessRequestBody(rc *extproc.RequestContext, body []byte) error {
	chain, err := p.chainFor(rc)
	if err != nil {
		return err
	}
	return chain.RunRequestBody(newStreamContext(rc), body)
}

// ProcessRequestTrailers implements extproc.RequestProcessor: forwards the
// request-trailers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessRequestTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	chain, err := p.chainFor(rc)
	if err != nil {
		return err
	}
	return chain.RunRequestTrailers(newStreamContext(rc), trailers)
}

// ProcessResponseHeaders implements extproc.RequestProcessor: forwards the
// response-headers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseHeaders(rc *extproc.RequestContext, headers extproc.AllHeaders) error {
	chain, err := p.chainFor(rc)
	if err != nil {
		return err
	}
	return chain.RunResponseHeaders(newStreamContext(rc), headers)
}

// ProcessResponseBody implements extproc.RequestProcessor: forwards the
// response-body phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseBody(rc *extproc.RequestContext, body []byte) error {
	chain, err := p.chainFor(rc)
	if err != nil {
		return err
	}
	return chain.RunResponseBody(newStreamContext(rc), body)
}

// ProcessResponseTrailers implements extproc.RequestProcessor: forwards the
// response-trailers phase to the stream's resolved chain.
func (p *ChainProcessor) ProcessResponseTrailers(rc *extproc.RequestContext, trailers extproc.AllHeaders) error {
	chain, err := p.chainFor(rc)
	if err != nil {
		return err
	}
	return chain.RunResponseTrailers(newStreamContext(rc), trailers)
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
		if ref := p.current(); ref != nil {
			// GracefulStop has already drained streams; force-close the live
			// generation. Any retired generation is closed by its last
			// StreamEnd, and this path is only reached on process exit.
			ref.close()
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

	// checkpoint is the mutation state captured by BeginPlugin; it lets the
	// chain roll back a plugin that failed under fail_open.
	checkpoint        extproc.PhaseSnapshot
	abortedCheckpoint bool
	hasCheckpoint     bool
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

func (s *streamContext) Context() context.Context { return s.rc.Context() }

func (s *streamContext) RequestInfo() plugin.RequestInfo {
	return plugin.RequestInfo{
		Scheme:    s.rc.Scheme,
		Authority: s.rc.Authority,
		Method:    s.rc.Method,
		Path:      s.rc.Path,
		FullPath:  s.rc.FullPath,
		RequestID: s.rc.RequestID,
		TraceID:   s.rc.TraceID,
		SpanID:    s.rc.SpanID,
	}
}

func (s *streamContext) ClearRouteCache() { s.rc.ClearRouteCache() }

func (s *streamContext) BeginPlugin() {
	s.checkpoint = s.rc.SnapshotPhase()
	s.abortedCheckpoint = s.aborted
	s.hasCheckpoint = true
}

func (s *streamContext) RollbackPlugin() {
	if !s.hasCheckpoint {
		return
	}
	s.rc.RestorePhase(s.checkpoint)
	s.aborted = s.abortedCheckpoint
	s.hasCheckpoint = false
}
