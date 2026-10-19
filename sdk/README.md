# sdk — the in-tree ext_proc server SDK

This package is forked from
[wrossmorrow/envoy-extproc-sdk-go](https://github.com/wrossmorrow/envoy-extproc-sdk-go)
and refactored for production use. It is maintained in-tree and the external
dependency has been dropped.

The public API is compatible with the original (`RequestContext`,
`RequestProcessor`, `Serve`, `AllHeaders`, ...), so migrating is just an
import-path change.

## Improvements over the original

| Improvement | Details |
|---|---|
| **panic recovery** | In the original SDK a processor panic propagated through the gRPC handler and killed the whole process. Recovery is now built into the phase dispatch and the error-notification path: a panic terminates only the current stream (Envoy applies its failure policy), and a new `extproc_panic_recoveries_total` metric tracks occurrences. |
| **plugin-level rollback** | The responsibility chain snapshots the phase mutations before each plugin. A `fail_open` plugin that returns an error or panics is rolled back before the chain continues, so partial header/body changes and request-scoped values never reach Envoy or later plugins. |
| **stream context** | `RequestContext.Context()` exposes the gRPC stream context; plugins can pass it to downstream calls and observe cancellation/message timeouts. The plugin bridge exposes it as `plugin.Context.Context()`. |
| **TLS/mTLS + bounds** | `ServerOptions` supports `BindAddress`, `MetricsBindAddress`, TLS/mTLS (`TLSOptions`), and explicit gRPC `MaxRecvMsgSize`/`MaxSendMsgSize`, so sidecar (loopback) and cross-pod (TLS + NetworkPolicy) layouts share one binary. `TLSOptions.ReloadIntervalSeconds` polls certificate/CA files and swaps them atomically. |
| **health and metrics** | The HTTP port serves `/metrics`, `/healthz` and `/readyz`; the gRPC health service implements `Watch`, so drain transitions are observable. Stream duration has classic histogram buckets in addition to the native histogram. |
| **secure default** | `NewDefaultProcessingOptions` sets `AbortOnProcessorFailure=true` (fail-closed); callers that want fail-open must opt in explicitly. |
| **per-stream lifecycle** | `RequestProcessor` implementations may also implement `StreamLifecycle` (`StreamStart`/`StreamEnd`). The loop always calls `StreamEnd`, including cancellation and panic paths, so processors can pin and release per-generation resources safely. |
| **plugin metrics** | `Metrics.PluginDuration` and `Metrics.PluginFailures` are labeled by `plugin` and `phase`; `ServeWithMetrics` lets a processor's observer share the same registry. |
| **trace context** | W3C `traceparent` is parsed into `RequestContext.TraceID`/`SpanID`, metadata logs and `plugin.Context.RequestInfo()`; malformed values are ignored and no exporter dependency is added. |
| **TLS rotation** | `TLSOptions.ReloadIntervalSeconds` polls certificate/CA files and atomically swaps the material; failed reloads keep the previous configuration. |
| **documented semantics** | Key constraints are explicit in comments: `ReplaceBody` vs `ReplaceBodyChunk` Content-Length behavior, no immediate response in trailer phases, per-phase mutation resets, etc. |

Still depends on `envoyproxy/go-control-plane` (generated ext_proc protocol
types) and `google.golang.org/grpc` — protocol types follow the official Envoy
repository; we do not maintain generated code.

## Layout

| File | Contents |
|---|---|
| `context.go` | Phase constants, `RequestContext` (pseudo-header parsing / mutation accumulation / immediate responses / cross-phase KV), `GetResponse` fallback semantics |
| `headers.go` | `AllHeaders`: case-insensitive, multi-value, order-preserving header accessors |
| `extproc.go` | `RequestProcessor` interface + `GenericExtProcServer` stream loop (metrics / empty-response fallback / panic guards) |
| `server.go` | `Serve`: metrics port + gRPC service + three-step graceful drain |
| `options.go` | `ServerOptions` / `ProcessingOptions` |
| `metrics.go` | The `extproc_*` metric set |
| `health.go` | gRPC health check (MarkReady / MarkUnready / Watch) |
| `trace.go` | Strict W3C `traceparent` parsing |
| `lifecycle.go` | Optional `StreamLifecycle` hooks for per-stream setup/teardown |

## Usage

```go
type MyProcessor struct{ /* implement the six phase callbacks */ }

func (p *MyProcessor) GetName() string                        { return "my-processor" }
func (p *MyProcessor) GetOptions() *extproc.ProcessingOptions { return p.opts }
func (p *MyProcessor) Close(int32) error                      { return nil }
// ... six phase callbacks ...

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
    extproc.MustServe(extproc.NewDefaultServerOptions(), &MyProcessor{}, logger)
}
```

`internal/processor` in this repository — the bridge to the plugin chain — is a
complete, production-shaped example built on this SDK.
