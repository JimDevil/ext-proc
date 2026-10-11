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
| `health.go` | gRPC health check (MarkReady / MarkUnready) |

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
