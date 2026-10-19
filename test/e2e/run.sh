#!/usr/bin/env bash
# End-to-end assertion: Envoy ext_proc chain injects model and API-key headers.
set -euo pipefail

cd "$(dirname "$0")/../.."
compose=(docker compose -f test/e2e/docker-compose.yaml)

cleanup() { "${compose[@]}" down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

"${compose[@]}" up -d --build

for _ in $(seq 1 60); do
  if curl -fsS http://127.0.0.1:19090/readyz >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS http://127.0.0.1:19090/readyz >/dev/null

response=$(curl -fsS http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: e2e-test-key' \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"hello"}]}')

echo "$response" | grep -qi 'x-higress-llm-model'
echo "$response" | grep -qi 'glm-5.3'
echo "$response" | grep -qi 'x-extracted-apikey'
echo "e2e OK"
