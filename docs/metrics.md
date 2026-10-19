# Metrics

`/metrics` (default `:19090`) exposes the SDK metric set. All names use the
`extproc_` prefix; counters end in `_total`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `extproc_active_streams` | gauge | — | Streams currently being processed |
| `extproc_streams_total` | counter | — | Streams started |
| `extproc_errored_streams_total` | counter | — | Streams that errored (once per stream) |
| `extproc_phase_errors_total` | counter | — | Phase-level processing errors (may repeat within a stream) |
| `extproc_panic_recoveries_total` | counter | — | Recovered processor/plugin panics |
| `extproc_stream_duration_seconds` | histogram | — | Stream duration (native + classic buckets) |
| `extproc_plugin_duration_seconds` | histogram | `plugin`, `phase` | Plugin execution latency |
| `extproc_plugin_failures_total` | counter | `plugin`, `phase`, `policy` | Plugin errors/panics; `policy` is `fail_open` or `fail_closed` |
| `extproc_request_headers_total` | counter | — | Request-headers phases |
| `extproc_request_body_messages_total` | counter | — | Request-body messages |
| `extproc_request_trailers_total` | counter | — | Request-trailers phases |
| `extproc_response_headers_total` | counter | — | Response-headers phases |
| `extproc_response_body_messages_total` | counter | — | Response-body messages |
| `extproc_response_trailers_total` | counter | — | Response-trailers phases |
| `extproc_empty_responses_total` | counter | — | Fallback empty continue responses |
| `extproc_request_body_bytes_total` | counter | — | Request body bytes received |
| `extproc_response_body_bytes_total` | counter | — | Response body bytes received |
| `extproc_response_send_errors_total` | counter | — | Failed gRPC sends to Envoy |

## Health endpoints

- `/healthz` — liveness; always 200 while the process is running.
- `/readyz` — readiness; 200 while serving, 503 after SIGTERM starts draining.
- gRPC `grpc.health.v1.Health/Check` and `Watch` — standard health protocol.

## Example queries

```promql
# p99 stream duration
histogram_quantile(0.99, sum by (le) (rate(extproc_stream_duration_seconds_bucket[5m])))

# plugin failure rate by plugin and policy
sum by (plugin, policy) (rate(extproc_plugin_failures_total[5m]))

# fail-open share of plugin failures
sum(rate(extproc_plugin_failures_total{policy="fail_open"}[5m]))
  / clamp_min(sum(rate(extproc_plugin_failures_total[5m])), 1)
```

Alerting rules live in [`manifest/deploy/prometheus-rules.yaml`](../manifest/deploy/prometheus-rules.yaml).
A ready-to-import Grafana dashboard is in
[`manifest/deploy/grafana-dashboard.json`](../manifest/deploy/grafana-dashboard.json).

## Trace context

The SDK parses the W3C `traceparent` request header and exposes the trace and
parent span IDs through `RequestContext.TraceID`/`SpanID`, metadata debug logs
(`traceId`) and `plugin.Context.RequestInfo()`. Malformed headers are ignored.
No exporter is bundled; the IDs are ready for your logging/tracing pipeline.
