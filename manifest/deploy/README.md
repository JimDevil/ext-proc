# Deployment modes

ext-proc supports both layouts; the difference is the listen address, the
security boundary and how Envoy discovers it.

| | Same-Pod sidecar | Cross-Pod Deployment |
|---|---|---|
| Manifests | `sidecar/deployment-patch.yaml` + `sidecar/config.yaml` | `deployment.yaml` |
| `server.bindAddress` | `127.0.0.1` | `0.0.0.0` |
| `server.metricsBindAddress` | `127.0.0.1` | `0.0.0.0` (scraped by monitoring) |
| Envoy cluster | `127.0.0.1:50051` (h2c) | `ext-proc.<ns>.svc:50051` |
| TLS | Not needed on loopback; terminate at the gateway | Enable `server.tls`; use `clientCAFile` for mTLS |
| NetworkPolicy | Not needed for ingress | Required: only the gateway and monitoring namespaces |
| Scaling | Scales with the gateway Pod | Independent replicas + PDB/HPA |
| Failure domain | Coupled to the gateway Pod | Independent; recommended for production |

## Sidecar

1. Create a ConfigMap from `sidecar/config.yaml` named `ext-proc-sidecar-config`.
2. Apply `sidecar/deployment-patch.yaml` to the gateway Deployment (or use it
   as a kustomize patch).
3. Point the Envoy ext_proc cluster at `127.0.0.1:50051` with
   `http2_protocol_options` (h2c). No Service is required.

The sidecar still exposes gRPC health on loopback, so the Pod's readiness can
include the gateway and ext-proc probes. `/metrics`, `/healthz` and `/readyz`
are on `127.0.0.1:19090`; scrape them through the Pod annotation or a sidecar
scraper.

## Cross-Pod

1. Create the TLS Secret and set `server.tls.enabled: true`,
   `certFile`/`keyFile` to the mounted paths and `clientCAFile` for mTLS.
2. Apply `deployment.yaml` (adjust the NetworkPolicy namespace/pod labels).
3. Point Envoy's ext_proc cluster at the Service DNS name and configure
   `transport_socket` with the same CA/SAN. Enable `failure_mode_allow: false`
   for auth-critical chains.
4. Keep an HPA/PDB in place; the process drains on SIGTERM after marking
   `/readyz` and the gRPC health check unready.

## Metrics and alerts

The HTTP port exposes `/metrics`, `/healthz` and `/readyz`. Plugin-level
metrics are `extproc_plugin_duration_seconds{plugin,phase}` and
`extproc_plugin_failures_total{plugin,phase,policy}`; stream-level metrics
include `extproc_streams_total`, `extproc_errored_streams_total`,
`extproc_stream_duration_seconds` and `extproc_panic_recoveries_total`.

Apply `prometheus-rules.yaml` (Prometheus Operator) and adjust the `job` label
to match your scrape configuration. Import `grafana-dashboard.json` into
Grafana and select the Prometheus datasource; the dashboard uses a `$job`
variable defaulting to `ext-proc`.

## TLS files

The server reads certificate files at startup. Set
`server.tls.reloadIntervalSeconds` (for example `30`) to poll the files and
swap certificate/CA material atomically; a failed reload keeps the previous
material. With `0`, rotate by rolling the Pods after updating the Secret.
