package plugin

import "time"

// PluginResult is one plugin invocation outcome reported to an Observer.
// Err is non-nil for a returned error or a recovered panic; FailOpen reports
// which policy handled it.
type PluginResult struct {
	Plugin   string
	Phase    string
	Duration time.Duration
	Err      error
	FailOpen bool
	Panicked bool
}

// Observer receives per-plugin execution metrics. It is optional: a nil
// observer disables instrumentation. Implementations must be cheap and
// concurrency-safe because chains run on many stream goroutines.
type Observer interface {
	ObservePlugin(PluginResult)
}
