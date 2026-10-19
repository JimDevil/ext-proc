# Security policy

## Reporting a vulnerability

Please use GitHub's private security advisory flow
("Security" -> "Report a vulnerability") instead of opening a public issue.
Include the affected version, a minimal reproduction, and the expected impact.
We will acknowledge the report and coordinate a fix and disclosure date.

## Supported versions

Security fixes are applied to the latest release on `main`. Older tags are
supported on a best-effort basis; pin images by digest for production.

## Security invariants

The project treats the following as invariants. A change that breaks one of
them is a security regression:

- Raw HTTP headers and bodies are never written to logs. Runtime logs contain
  metadata only (phase, request id, method, path, body length).
- Header names and values written by plugins are validated (token grammar, no
  CR/LF/NUL, no Envoy-protected headers). HTTP status codes are validated.
- Cross-pod deployments use TLS or mTLS; the plaintext profile is only for a
  same-Pod loopback sidecar. The server logs a warning when it starts without
  TLS on a non-loopback address.
- Authentication failures use `CancelRequest` (an immediate local reply), not
  a processor error alone: Envoy's `failure_mode_allow` can override a gRPC
  error.
- Configuration is validated at startup and on reload. A failed reload keeps
  the previous known-good generation.
- Credentials are read from environment variables or a secret manager. The
  repository, examples and tests contain no usable credential literals.
- The container runs as a non-root numeric user, with a read-only root
  filesystem in the example manifests.

## Deployment checklist

- Same-Pod sidecar: `bindAddress: 127.0.0.1`, no Service, no ingress
  NetworkPolicy required.
- Cross-pod: enable `server.tls` (prefer `clientCAFile` mTLS), apply a
  NetworkPolicy that admits only the gateway and monitoring namespaces, and
  pin the image by digest.
- Set `failure_mode_allow: false` for auth-critical ext_proc chains, or use an
  immediate response for unconditional rejection.
- Keep `logger.level: info` in production.
