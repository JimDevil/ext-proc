# Benchmark harness (ext_proc chain vs wasmPlugin)

Reproduces the numbers in [docs/benchmark.md](../../docs/benchmark.md).

## Layout

| Path | What |
|---|---|
| `perf-upstream/` | Streaming upstream: SSE with an immediate first byte (TTFT-sensitive), echoes injected plugin headers back as `x-echo-*` |
| `loadtest/` | Go load generator measuring TTFT (first SSE body byte) and total latency |
| `configmap.yaml` | ext-proc config used by the benchmark (both plugins, `info` logging) |
| `extproc-perf.yaml` | ext-proc Deployment + Service + upstream Deployment/Service + Ingress (`perf.test`) |
| `wasmplugin.yaml` | the comparison WasmPlugin (official higress model-router 2.0.3) |
| `matrix.sh` | 3 interleaved rounds of baseline / wasm / ext2 at 16 concurrency × 30s |
| `saturation.sh` | one round per scenario at 64 concurrency × 20s |
| `results-2026-10-12/` | raw per-round JSON of the published numbers |

`EnvoyFilter` lives in [`manifest/envoyfilter-demo.yaml`](../envoyfilter-demo.yaml) — it
points at the in-cluster ext-proc service (`STRICT_DNS`).

## Run

```shell
# 0. build images (OrbStack docker; mirror busybox via daocloud if needed)
docker build -t extproc:perf -f Dockerfile .
cd manifest/benchmark/perf-upstream && docker build -t perf-upstream:1 . && cd -

# 1. deploy the benchmark env
kubectl -n higress-cj apply -f manifest/benchmark/

# 2. load generator (Mac, against the gateway NodePort)
cd manifest/benchmark/loadtest && go build -o loadtest . && cd -

# 3. matrix + saturation
manifest/benchmark/matrix.sh
manifest/benchmark/saturation.sh

# 4. aggregate (median across rounds) and compare with results-2026-10-12/
```

Known caveat: editing the ext-proc ConfigMap does NOT hot-reload the chain —
GoFrame gcfg watches the file inode, while kubelet swaps ConfigMap mounts via
a symlink rename that fsnotify does not report. Restart the deployment after
ConfigMap changes (`kubectl -n higress-cj rollout restart deploy/extproc-perf`).
