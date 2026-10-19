package processor

import (
	"sync/atomic"

	extproc "github.com/JimDevil/ext-proc/sdk"

	"github.com/JimDevil/ext-proc/internal/plugin"
)

// MetricsObserver bridges the plugin chain's per-invocation results into the
// SDK metric set. The metrics are installed after the processor is built (the
// HTTP server owns the registry), hence the atomic pointer.
type MetricsObserver struct {
	metrics atomic.Pointer[extproc.Metrics]
}

// SetMetrics installs the metric set; nil disables instrumentation.
func (o *MetricsObserver) SetMetrics(metrics *extproc.Metrics) {
	o.metrics.Store(metrics)
}

// ObservePlugin implements plugin.Observer.
func (o *MetricsObserver) ObservePlugin(result plugin.PluginResult) {
	metrics := o.metrics.Load()
	if metrics == nil {
		return
	}
	metrics.PluginDuration.WithLabelValues(result.Plugin, result.Phase).Observe(result.Duration.Seconds())
	if result.Panicked {
		// The chain recovers plugin panics before they reach the SDK loop, so
		// account for them here to keep extproc_panic_recoveries_total
		// meaningful for operators.
		metrics.PanicRecoveries.Inc()
	}
	if result.Err == nil {
		return
	}
	policy := "fail_closed"
	if result.FailOpen {
		policy = "fail_open"
	}
	metrics.PluginFailures.WithLabelValues(result.Plugin, result.Phase, policy).Inc()
}

var _ plugin.Observer = (*MetricsObserver)(nil)
