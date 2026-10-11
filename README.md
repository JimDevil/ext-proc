# ext-proc

**English** | [简体中文](README.zh-CN.md)

Move gateway plugins out of WASM — a responsibility-chain plugin framework built on [Envoy](https://www.envoyproxy.io)'s [ext_proc](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter) filter.

Every plugin is just a Go package behind a standard gRPC service: deploy and scale independently, crashes never take down Envoy. Hot-reloadable config, chain orchestration, and two LLM-gateway plugins out of the box.

> **💡 Use it as a scaffold.** This repo is a ready-to-clone starting point for developing your own ext_proc filter: clone it, remove the two sample plugins (their packages under `internal/plugins/`, their imports in `main.go`, and their segments in `config.yaml` — or simply set `enabled: false`), then develop your business logic in `internal/plugins/<your-plugin>`. Everything a data-plane service needs — chain orchestration, hot reload, metrics, graceful shutdown, panic recovery — is handled by the framework. The two sample plugins double as working reference implementations: keep them, study them, or delete them. See [Write your own plugin](#write-your-own-plugin).

## Why leave WASM behind

WASM plugins live inside the Envoy process — memory, failures and operations are all coupled. ext_proc moves plugin logic into an ordinary process; the price is one gRPC round-trip per processing phase.

| Dimension | WASM plugin | ext-proc |
|---|---|---|
| Memory & tail latency | WASM VM rebuilds at a ~200MB threshold — the rebuild window drops headers and blocks workers; a cooldown turns it into a storm | **No VM: no rebuild window**; config hot-reload swaps the chain atomically |
| Failure domain | A plugin panic cuts the stream (`503 wasm_fail_stream`); FAIL_OPEN silently passes traffic | Process isolation: a crash only affects in-flight streams; **explicit fail-open / fail-closed** |
| Language & debugging | TinyGo constraints; wasm logs are a black box | Any gRPC language; standard Kubernetes tooling — observable and debuggable |
| Operations | Embedded in the Envoy process, released with the gateway | Standalone service: HPA, PDB, graceful drain and canary releases come for free |
| Streaming | Chunked HttpBody callbacks; large bodies are fragile | `FULL_DUPLEX_STREAMED` mode is a natural fit for LLM SSE passthrough |

## Architecture

```
 Client                      Envoy Gateway                          ext-proc
┌───────┐   request    ┌──────────────────────┐   bidirectional  ┌───────────────────────┐
│ curl/ │ ──────────▶  │  envoy.filters.http  │ ──────gRPC─────▶ │  responsibility chain │
│ SDK   │              │  .ext_proc           │ ◀──────────────  │  ├─ model-router      │
└───────┘              │  (5 phases: headers, │    mutations     │  └─ apikey-extract    │
                       │   body, trailers)    │                  │  (+ your plugins)     │
                       └──────────┬───────────┘                  └───────────────────────┘
                                  │ route / forward (with injected headers)
                                  ▼
                              Upstream
```

One stream per request · one goroutine per stream · an `ImmediateResponse` short-circuits the chain · two-layer fail-open / fail-closed.

![The plugin chain — matchRules routing, per-plugin failure policy, short-circuit, and atomic hot swap](docs/assets/chain-architecture.svg)

## Core features

- **Chain orchestration** — plugins run in the configured `order`; the first short-circuit stops the rest. Plugins share nothing but request-scoped state.
- **Standard plugin API** — six phase callbacks + `BasePlugin` defaults + self-registration in `init()`: one package, five steps, zero coupling.
- **Hot reload** — GoFrame gcfg watches `config.yaml` and rebuilds the chain atomically; in-flight requests keep the old chain. Zero restarts.
- **Two-layer failure policy** — chain-level `fail_open` / `fail_closed` mirrors Envoy's `failure_mode_allow`, and each plugin can override it (`plugins[].failurePolicy`): auth plugins fail-closed while routing helpers fail-open, in the same chain. Every silent fallback is visible in metrics.
- **Observability** — Prometheus metrics (streams, phase errors, duration histograms), gRPC health checks, structured slog logging; per-phase latency is available to Envoy access logs via filter state.
- **Production engineering** — three-step graceful drain (unready → propagation delay → bounded stop); gRPC flow control delegated to Envoy backpressure; **panic recovery** — a processor panic terminates only the current stream, tracked by `extproc_panic_recoveries_total`.

## Sample plugins

### `model-router` *(migrated from the higress model-router plugin)*

Extracts the model name from the request body and injects a routing header — the foundation for model routing and multi-provider gateways.

- **JSON / multipart**: on-demand extraction with gjson, no full parse; multipart form fields supported too
- **`provider/model` prefix split**: the provider goes into its own header; the body model name can be de-prefixed (`keepOriginalModelName`)
- **`higress/auto` routing**: regex rules match the last user message; falls back to `defaultModel`

### `apikey-extract`

Normalizes API keys scattered across sources into one target request header for downstream plugins and upstreams.

- **Ordered fallback sources**: `x-api-key` header → `Authorization: Bearer` → `apikey` query parameter
- **Leak prevention**: `stripSource` deletes the source header (query sources can't be removed by protocol)
- **Required mode**: `401` rejection when no source matches; the body is JSON-encoded to prevent injection

## Route-scoped configuration (matchRules)

Like higress wasmPlugin `matchRules` — but owned **per plugin**: each plugin carries its own rules, resolved independently per request (one plugin on a rule override while another stays global is the normal case). Rules are evaluated in order — **first match wins** — and unmatched requests use the plugin's global config:

```yaml
plugins:
- name: model-router
  order: 10
  config: { modelToHeader: x-higress-llm-model }   # global default
  matchRules:
  - domain: ["api.example.com"]
    pathPrefix: /v1/chat
    config: { modelToHeader: x-model-chat }        # deep-merged over the global config
  - domain: ["*.internal.corp"]
    disable: true                                  # plugin off for this domain
- name: apikey-extract
  order: 20
  matchRules:
  - pathPrefix: /internal/
    disable: true
```

- `domain` supports exact names, `*.wildcard`, or `*` (the port is stripped before matching); `pathPrefix` is a prefix match — a rule matches when **all** configured dimensions match.
- Rule `config` deep-merges over the plugin's global config; `disable: true` switches the plugin off for the matched routes (other plugins unaffected).
- Each plugin's variants are pre-built at config load (one instance per rule) and the whole table is swapped atomically on hot reload — in-flight requests finish on the old table, new requests immediately use the matched variants.

## Quick start

```shell
# 1. Build and run (dependencies vendored — offline builds work)
git clone https://github.com/JimDevil/ext-proc.git && cd ext-proc
make vendor && make build
./ext-proc   # gRPC :50051 · Prometheus :19090/metrics
```

Wire it into Envoy (the essential part — see also `manifest/envoyfilter-demo.yaml` for the Istio/Higress `EnvoyFilter` form):

```yaml
http_filters:
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    stat_prefix: ext_proc
    grpc_service:
      envoy_grpc: { cluster_name: ext-proc-cluster }
    processing_mode:
      request_header_mode: SEND
      request_body_mode: BUFFERED      # plugins receive the complete JSON/multipart
      response_header_mode: SEND
      response_body_mode: NONE
    message_timeout: 2s
    failure_mode_allow: false          # auth/routing plugins want fail-closed
```

```shell
# 2. Send a request and watch the injected headers
curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-key" \
  -d '{"model":"glm-5.3","messages":[{}]}'

# Headers seen by the upstream (visible in echo services / access logs):
#   x-higress-llm-model: glm-5.3      <- injected by model-router
#   x-extracted-apikey:  your-key     <- injected by apikey-extract
```

### Docker

Multi-stage `Dockerfile`: static binary built offline from the vendored deps, scratch runtime with CA certs, non-root, ~10MB. Multi-arch ready (`TARGETARCH`-aware):

```shell
make image          # build for the local platform + smoke-run the container
make image-multi    # build & push linux/amd64 + linux/arm64 (needs buildx + registry login)
```

## Write your own plugin

Plugins have zero coupling to each other, so different people can develop different plugins in parallel:

1. **Create a package** `internal/plugins/<name>`; embed `plugin.BasePlugin` and override only the phases you need.
2. **Implement the interface** — the six phase callbacks on demand; `Init` parses your own config section and fills defaults.
3. **Self-register** — `plugin.Register` in `init()`, plus one anonymous import in `main.go`.
4. **Add a config segment** — `name/enabled/order/config` under `chain.plugins` in `config.yaml`.
5. **Table-driven tests** — assert header/body operations with the `plugintest` mock Context; keep `make test` green.

```go
package myplugin

type MyPlugin struct {
	plugin.BasePlugin // override only the phases you need
}

func New() plugin.Plugin { return &MyPlugin{} }
func init()              { plugin.Register("my-plugin", New) }

func (m *MyPlugin) Name() string { return "my-plugin" }

func (m *MyPlugin) OnRequestHeaders(ctx plugin.Context, h extproc.AllHeaders) error {
	// Short-circuit with a local reply; returning nil continues the chain.
	if _, blocked := h.Get("x-block"); blocked {
		return ctx.CancelRequest(403, nil, "blocked by my-plugin")
	}
	return nil
}
```

```yaml
chain:
  failurePolicy: fail_closed
  plugins:
    - name: model-router
      enabled: true
      order: 10
      config: { modelToHeader: x-higress-llm-model }
    - name: my-plugin
      enabled: true
      order: 20
      config: { yourKey: yourValue }   # plugin-private config, parsed by the plugin itself
```

## Notes

- **Hot reload**: editing `config.yaml` rebuilds the chain automatically (new requests use the new chain, in-flight requests the old one). Plugin instances are immutable — never keep cross-request mutable state on an instance.
- **Envoy-protected headers**: `:method`, `:authority`, `host` and `x-envoy-*` cannot be rewritten; Envoy rejects those mutations.
- **Content-Length**: Envoy strips it automatically when a plugin mutates the body; plugins never need to touch it.
- **Large bodies**: under `BUFFERED` the body is limited by Envoy's `max_message_bytes`; for very large requests use `STREAMED` and parse incrementally. Both sample plugins assume BUFFERED full-body semantics (aligned with their higress originals).
- **SDK**: the ext_proc server SDK lives in-tree at [`sdk/`](sdk/) (forked from [wrossmorrow/envoy-extproc-sdk-go](https://github.com/wrossmorrow/envoy-extproc-sdk-go), refactored and maintained here — see its README for the changes).

## License

Apache-2.0
