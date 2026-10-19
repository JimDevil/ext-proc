#!/bin/bash
# Interleaved benchmark matrix (see docs/benchmark.md): baseline / wasm /
# ext2, 3 rounds. Run after applying manifest/benchmark/configmap.yaml,
# extproc-perf.yaml and the Ingress (host perf.test -> perf-upstream).
#
#   baseline : no EnvoyFilter, no WasmPlugin
#   wasm     : WasmPlugin model-router-perf only
#   ext2     : EnvoyFilter ext-proc-demo only (2-plugin chain)
RES=/tmp/perf-manifests/results
mkdir -p $RES
LT=/tmp/perf-loadtest/loadtest
K="kubectl --context orbstack -n higress-cj"

verify() { # $1 = expected: 1 = model header injected, 0 = absent
  local want=$1 host=perf.test
  for i in $(seq 1 20); do
    local got
    got=$(curl -s --max-time 6 -H "Host: $host" -H "Content-Type: application/json" -H "x-api-key: v" \
      -D - -o /dev/null http://localhost:32356/chat/completions -d '{"model":"gpt-4o","perf_chunks":2}' \
      | grep -c "x-echo-x-higress-llm-model")
    if [ "$got" -ge 1 ] && [ "$want" = "1" ]; then return 0; fi
    if [ "$got" = "0" ] && [ "$want" = "0" ]; then return 0; fi
    sleep 3
  done
  return 1
}

for round in 1 2 3; do
  # --- baseline: no envoyfilter, no wasmplugin ---
  $K delete envoyfilter ext-proc-demo --wait=false >/dev/null 2>&1
  $K delete wasmplugin model-router-perf --wait=false >/dev/null 2>&1
  verify 0 || { echo "round$round baseline: NOT clean"; exit 1; }
  $LT -seconds 30 -warmup 5 > $RES/baseline-r$round.json
  echo "round$round baseline done: $(grep -o '"p50": [0-9.]*' $RES/baseline-r$round.json | head -1)"

  # --- wasm: official model-router plugin ---
  $K apply -f /tmp/perf-manifests/wasmplugin.yaml >/dev/null
  verify 1 || { echo "round$round wasm: plugin not active"; exit 1; }
  $LT -seconds 30 -warmup 5 > $RES/wasm-r$round.json
  echo "round$round wasm done: $(grep -o '"p50": [0-9.]*' $RES/wasm-r$round.json | head -1)"

  # --- ext2: envoyfilter + 2-plugin chain ---
  $K delete wasmplugin model-router-perf --wait=false >/dev/null 2>&1
  $K apply -f /tmp/perf-manifests/envoyfilter-perf.yaml >/dev/null
  verify 1 || { echo "round$round ext2: chain not active"; exit 1; }
  $LT -seconds 30 -warmup 5 > $RES/ext2-r$round.json
  echo "round$round ext2 done: $(grep -o '"p50": [0-9.]*' $RES/ext2-r$round.json | head -1)"
done
echo MATRIX-DONE
