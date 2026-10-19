package extproc

import (
	"reflect"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the built-in metric set (extproc_* prefix); register it with any
// prometheus.Registerer via Register.
type Metrics struct {
	ActiveStreams         prometheus.Gauge
	ErroredStreams        prometheus.Counter
	PhaseErrors           prometheus.Counter
	PanicRecoveries       prometheus.Counter
	TotalStreams          prometheus.Counter
	StreamDurationSeconds prometheus.Histogram
	// PhaseDurationSeconds is the per-phase processor duration (the groundwork
	// for phase-level tracing): one sample per processed phase message,
	// labeled by phase name.
	PhaseDurationSeconds  *prometheus.HistogramVec
	TotalRequestHeaders   prometheus.Counter
	TotalRequestBody      prometheus.Counter
	TotalRequestTrailers  prometheus.Counter
	TotalResponseHeaders  prometheus.Counter
	TotalResponseBody     prometheus.Counter
	TotalResponseTrailers prometheus.Counter
	TotalEmptyResponses   prometheus.Counter
	BodyBytesReceived     prometheus.Counter
	BodyBytesReturned     prometheus.Counter
	ResponseSendErrors    prometheus.Counter
	// PluginDuration is the per-plugin, per-phase execution latency.
	PluginDuration *prometheus.HistogramVec
	// PluginFailures counts plugin errors and panics. policy is fail_open or
	// fail_closed (the policy that handled the failure).
	PluginFailures *prometheus.CounterVec
}

func NewEmptyMetrics() *Metrics {
	return &Metrics{
		ActiveStreams: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "extproc_active_streams",
				Help: "Current number of active streams.",
			},
		),
		ErroredStreams: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_errored_streams_total",
				Help: "Total number of streams that errored, counted once per stream.",
			},
		),
		PhaseErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_phase_errors_total",
				Help: "Total number of phase processing errors, which may repeat within one stream.",
			},
		),
		// Enhancement over the original SDK: counts recovered processor panics
		// (a gRPC handler panic no longer kills the process).
		PanicRecoveries: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_panic_recoveries_total",
				Help: "Total number of processor or plugin panics recovered by the framework.",
			},
		),
		TotalStreams: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_streams_total",
				Help: "Total number of streams processed.",
			},
		),
		StreamDurationSeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:                        "extproc_stream_duration_seconds",
				Help:                        "Stream duration in seconds (native and classic histograms).",
				NativeHistogramBucketFactor: 1.1, // O(10%) resolution at any scale
				// Classic buckets keep the metric usable with text/OpenMetrics
				// scrapes; a native-only histogram exposes just +Inf there.
				Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			},
		),
		PhaseDurationSeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "extproc_phase_duration_seconds",
				Help: "Per-phase processor duration in seconds. Phase durations are microseconds-scale, so the buckets start at 10µs.",
				// 10µs .. ~5min exponential; classic buckets kept for scrapers
				// without native-histogram support.
				Buckets: prometheus.ExponentialBuckets(0.00001, 1.6, 25),
			},
			[]string{"phase"},
		),
		TotalRequestHeaders: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_request_headers_total",
				Help: "Total number of request headers processed.",
			},
		),
		TotalRequestBody: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_request_body_messages_total",
				Help: "Total number of request bodies processed.",
			},
		),
		TotalRequestTrailers: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_request_trailers_total",
				Help: "Total number of request trailers processed.",
			},
		),
		TotalResponseHeaders: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_response_headers_total",
				Help: "Total number of response headers processed.",
			},
		),
		TotalResponseBody: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_response_body_messages_total",
				Help: "Total number of response bodies processed.",
			},
		),
		TotalResponseTrailers: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_response_trailers_total",
				Help: "Total number of response trailers processed.",
			},
		),
		TotalEmptyResponses: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_empty_responses_total",
				Help: "Total number of empty responses processed.",
			},
		),
		BodyBytesReceived: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_request_body_bytes_total",
				Help: "Total number of body bytes received.",
			},
		),
		BodyBytesReturned: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_response_body_bytes_total",
				Help: "Total number of body bytes returned.",
			},
		),
		ResponseSendErrors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "extproc_response_send_errors_total",
				Help: "Total number of response send errors.",
			},
		),
		PluginDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "extproc_plugin_duration_seconds",
				Help:    "Plugin execution duration in seconds, labeled by plugin and phase.",
				Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
			},
			[]string{"plugin", "phase"},
		),
		PluginFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "extproc_plugin_failures_total",
				Help: "Total number of plugin failures, labeled by plugin, phase and handling policy.",
			},
			[]string{"plugin", "phase", "policy"},
		),
	}
}

// Collectors reflects over all metric fields (for registrars).
func (m *Metrics) Collectors() []prometheus.Collector {
	v := reflect.ValueOf(m).Elem()
	cs := make([]prometheus.Collector, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		// Interface() panics on unexported fields
		if !field.CanInterface() {
			continue
		}
		// Skip typed nil pointers/interfaces: they satisfy the Collector type
		// assertion but would panic inside MustRegister.
		switch field.Kind() {
		case reflect.Ptr, reflect.Interface:
			if field.IsNil() {
				continue
			}
		}
		if c, ok := field.Interface().(prometheus.Collector); ok {
			cs = append(cs, c)
		}
	}
	return cs
}

// Register registers all metrics with the given Registerer and returns m
// (chainable).
func (m *Metrics) Register(r prometheus.Registerer) *Metrics {
	for _, c := range m.Collectors() {
		r.MustRegister(c)
	}
	return m
}
