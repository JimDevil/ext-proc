# End-to-end and load tests

The compose stack runs Envoy with the ext_proc filter, this repository's
ext-proc image, and an echo upstream.

```shell
# 1. start the stack (builds the ext-proc image from the repo)
docker compose -f test/e2e/docker-compose.yaml up -d --build

# 2. assert the chain injects headers
./test/e2e/run.sh

# 3. optional load test (requires k6)
k6 run test/e2e/loadtest.js

# 4. stop
docker compose -f test/e2e/docker-compose.yaml down -v
```

`run.sh` starts and stops the stack itself, so it can be used as a CI smoke
test on a Docker-capable runner. The k6 thresholds are intentionally strict
for a local echo upstream; adjust `p(95)`/`p(99)` after measuring your gateway.

The stack uses plaintext gRPC on the compose network; both the cross-pod and
sidecar production profiles are documented in
[`manifest/deploy/`](../../manifest/deploy/).
