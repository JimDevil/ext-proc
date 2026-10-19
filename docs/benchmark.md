# Benchmark: ext_proc plugin chain vs Higress wasmPlugin

Measured: 2026-10-12 · environment: single-node OrbStack k8s (arm64) on a
MacBook Pro, Higress 2.2.4 gateway (1 pod) · upstream: `perf-upstream` (Go,
SSE — response headers + first data chunk immediately, then 19 chunks at
10ms intervals) · load generator: Go client on the same Mac, keep-alive,
warmup 5s, measured windows below · request: POST /chat/completions,
~110B JSON body (model + one message) · 0 errors in every run.

The comparison pits the SAME semantic work (extract `model` from the request
body, inject `x-higress-llm-model`) in the two runtimes:

- **wasm**: the official higress `model-router` wasmPlugin
  (`oci://higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/model-router:2.0.3`, FAIL_CLOSE), loaded in the gateway process;
- **ext_proc**: this repo's 2-plugin chain (`model-router` + `apikey-extract`)
  served by an in-cluster ext-proc pod (sidecar-equivalent geometry), wired
  with an EnvoyFilter (`processing_mode: BUFFERED` body, `failure_mode_allow:
  false`).

Constant across all scenarios: a pre-existing `ai-statistics` wasmPlugin on
the gateway, the gateway and upstream pods, and the load client.

## Latency (16 concurrent workers, 30s, 3 interleaved rounds — median of rounds)

| Scenario | TTFT mean | TTFT p50 | TTFT p90 | TTFT p99 | Total p50 | RPS |
|---|---|---|---|---|---|---|
| baseline (no plugin) | 5.24 | 4.62 | 9.86 | 18.30 | 227.58 | 70.3 |
| wasmPlugin (model-router) | 4.93 | **4.06** | 9.25 | 16.28 | 226.69 | 70.6 |
| ext_proc (2-plugin chain) | 5.78 | **4.80** | 9.74 | 19.37 | 227.73 | 70.0 |

TTFT delta vs baseline (p50 / p90 / p99): wasm **−0.56 / −0.61 / −2.02 ms**,
ext_proc **+0.18 / −0.12 / +1.07 ms**. Per-round TTFT p50 spread was
3.5–6.7 ms, i.e. run noise is of the same order as the deltas — at this
scale neither runtime shows a measurable regression; ext_proc's p50 cost is
bounded at well under 1 ms.

## Saturation probe (64 concurrent workers, 20s, one round)

| Scenario | RPS | Δ RPS | TTFT p50 | TTFT p99 |
|---|---|---|---|---|
| baseline | 278.7 | — | 8.15 | 26.6 |
| wasmPlugin | 276.8 | −0.7% | 9.75 | 27.4 |
| ext_proc (2-plugin chain) | 270.0 | **−3.1%** | **16.43** | 56.0 |

Under CPU contention the ext_proc chain becomes visible: −3% throughput and
+8ms TTFT p50 — the cost of several gRPC phase round trips per request
(stream setup, header/body phases) plus extra scheduling on both sides. The
in-process wasmPlugin stays at baseline.

## Where the ext_proc time goes (self-timing)

`extproc_phase_duration_seconds` (added for phase-level tracing) on the
2-plugin chain, per-stream mean: REQUEST_HEADERS ≈ 20µs, REQUEST_BODY ≈
7.6µs, RESPONSE_HEADERS ≈ 10µs. The plugin work itself is microseconds —
the overhead measured above is dominated by the hop mechanics (per-phase
gRPC streams, context switching), not by plugin logic.

## Interpretation

1. **TTFT at realistic LLM scale**: real LLM TTFT is hundreds of ms to
   seconds; both runtimes add at most ~1ms here (0 at p50 for wasm, ≤1ms for
   a 2-plugin ext_proc chain at 70 rps). Plugin runtime choice is not a TTFT
   factor in practice.
2. **Throughput ceiling**: the ext_proc hop costs ~3% throughput at 64-way
   concurrency on one small node; wasm is flat. Under heavier bodies or more
   plugins the gap grows roughly linearly with phase round trips.
3. **What you buy for that cost** (the reason this repo exists): process
   isolation, no WASM VM rebuild storms/silent skips, native Go tooling,
   independent deployment, and per-phase observability.

## Caveats

- Single-node laptop environment; absolute numbers are not production
  numbers — the deltas between scenarios are the signal.
- The load generator shares the machine with the cluster (3 interleaved
  rounds mitigate drift; p99 remains noisy).
- ext-proc ran in-cluster (sidecar-equivalent); a remote/cross-node
  processor would add one network RTT per phase.
- Response-body processing was disabled (`response_body_mode: NONE`); a
  chain that inspects responses adds further phase round trips.
- gRPC connection reuse: the gateway keeps one HTTP/2 connection per
  processor endpoint; streams are multiplexed over it.

## Reproduce

`manifest/benchmark/` contains everything: the streaming upstream
(`perf-upstream/`), the Go load generator (`loadtest/`), the k8s manifests
(configmap, deployment, wasmplugin, EnvoyFilter is
`manifest/envoyfilter-demo.yaml` pointing at the in-cluster service), and
the driver scripts (`matrix.sh`, `saturation.sh`). Raw per-round JSON lives
in `results-2026-10-12/`.
