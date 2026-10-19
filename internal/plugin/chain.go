package plugin

import (
	"fmt"
	"log/slog"
	"time"

	extproc "github.com/JimDevil/ext-proc/sdk"
	"github.com/gogf/gf/v2/errors/gerror"
)

// pluginEntry is one position in the chain: the plugin instance plus its
// resolved failure policy (a plugin-level override wins over the chain
// default).
type pluginEntry struct {
	plugin   Plugin
	failOpen bool
}

// Chain is the responsibility chain: it holds the Plugin instances that are
// enabled and sorted by order, each with its own failure policy. The chain is
// immutable — hot reload rebuilds and atomically swaps it (see
// internal/processor) — so the Chain and its plugin instances are read-only at
// runtime and need no locking.
type Chain struct {
	entries  []pluginEntry
	logger   *slog.Logger
	observer Observer
}

// Names returns the plugin names on the chain (in execution order), for logs
// and troubleshooting.
func (c *Chain) Names() []string {
	names := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		names = append(names, e.plugin.Name())
	}
	return names
}

// runPhase executes the chain in order:
//   - every plugin runs inside a panic guard; a panic is converted to an error
//     and handled by THAT plugin's failure policy instead of escaping to the
//     SDK layer;
//   - fail_open rolls back the mutations written by the failed plugin, logs,
//     and skips it; fail_closed aborts (returns the error);
//   - the first plugin that short-circuits (CancelRequest) stops the rest.
func (c *Chain) runPhase(phase string, ctx Context, invoke func(Plugin) error) error {
	for _, e := range c.entries {
		ctx.BeginPlugin()
		started := time.Now()
		err, panicked := c.runPlugin(e, phase, invoke)
		if c.observer != nil {
			c.observer.ObservePlugin(PluginResult{
				Plugin:   e.plugin.Name(),
				Phase:    phase,
				Duration: time.Since(started),
				Err:      err,
				FailOpen: e.failOpen,
				Panicked: panicked,
			})
		}
		if err != nil {
			if e.failOpen {
				ctx.RollbackPlugin()
				c.logger.Error("plugin failed, rolling back and skipping per the fail_open policy",
					slog.String("phase", phase),
					slog.String("plugin", e.plugin.Name()),
					slog.String("error", err.Error()))
				continue
			}
			return gerror.Wrapf(err, "plugin %s failed in phase %s", e.plugin.Name(), phase)
		}
		if ctx.Aborted() {
			c.logger.Debug("plugin short-circuited the request, chain stops",
				slog.String("phase", phase), slog.String("plugin", e.plugin.Name()))
			break
		}
	}
	return nil
}

// runPlugin invokes one plugin and converts a panic into an error. The panic
// is recovered at the plugin boundary so one plugin's bug cannot bypass the
// chain's failure policy.
func (c *Chain) runPlugin(e pluginEntry, phase string, invoke func(Plugin) error) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			err = fmt.Errorf("plugin %s panicked in phase %s: %v", e.plugin.Name(), phase, r)
		}
	}()
	return invoke(e.plugin), false
}

// RunRequestHeaders runs the request-headers phase (pseudo headers :path/:method
// etc. are visible here).
func (c *Chain) RunRequestHeaders(ctx Context, headers extproc.AllHeaders) error {
	return c.runPhase("request_headers", ctx, func(p Plugin) error { return p.OnRequestHeaders(ctx, headers) })
}

// RunRequestBody runs the request-body phase (BUFFERED full body or STREAMED chunks).
func (c *Chain) RunRequestBody(ctx Context, body []byte) error {
	return c.runPhase("request_body", ctx, func(p Plugin) error { return p.OnRequestBody(ctx, body) })
}

// RunRequestTrailers runs the request-trailers phase.
func (c *Chain) RunRequestTrailers(ctx Context, trailers extproc.AllHeaders) error {
	return c.runPhase("request_trailers", ctx, func(p Plugin) error { return p.OnRequestTrailers(ctx, trailers) })
}

// RunResponseHeaders runs the response-headers phase.
func (c *Chain) RunResponseHeaders(ctx Context, headers extproc.AllHeaders) error {
	return c.runPhase("response_headers", ctx, func(p Plugin) error { return p.OnResponseHeaders(ctx, headers) })
}

// RunResponseBody runs the response-body phase.
func (c *Chain) RunResponseBody(ctx Context, body []byte) error {
	return c.runPhase("response_body", ctx, func(p Plugin) error { return p.OnResponseBody(ctx, body) })
}

// RunResponseTrailers runs the response-trailers phase.
func (c *Chain) RunResponseTrailers(ctx Context, trailers extproc.AllHeaders) error {
	return c.runPhase("response_trailers", ctx, func(p Plugin) error { return p.OnResponseTrailers(ctx, trailers) })
}
