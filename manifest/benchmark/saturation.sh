#!/bin/bash
# Saturation probe (see docs/benchmark.md): 1 round per scenario at 64
# concurrent workers, 20s each. Prerequisites as matrix.sh.
RES=/tmp/perf-manifests/results
LT=/tmp/perf-loadtest/loadtest
K="kubectl --context orbstack -n higress-cj"

verify() {
  local want=$1
  for i in $(seq 1 20); do
    local got
    got=$(curl -s --max-time 6 -H "Host: perf.test" -H "Content-Type: application/json" -H "x-api-key: v" \
      -D - -o /dev/null http://localhost:32356/chat/completions -d '{"model":"gpt-4o","perf_chunks":2}' \
      | grep -c "x-echo-x-higress-llm-model")
    if [ "$got" -ge 1 ] && [ "$want" = "1" ]; then return 0; fi
    if [ "$got" = "0" ] && [ "$want" = "0" ]; then return 0; fi
    sleep 3
  done
  return 1
}

$K delete envoyfilter ext-proc-demo --wait=false >/dev/null 2>&1
$K delete wasmplugin model-router-perf --wait=false >/dev/null 2>&1
verify 0 || exit 1
$LT -concurrency 64 -seconds 20 -warmup 5 > $RES/sat-baseline.json
echo "baseline: $(grep -o '"rps": [0-9.]*' $RES/sat-baseline.json)"

$K apply -f /tmp/perf-manifests/wasmplugin.yaml >/dev/null
verify 1 || exit 1
$LT -concurrency 64 -seconds 20 -warmup 5 > $RES/sat-wasm.json
echo "wasm: $(grep -o '"rps": [0-9.]*' $RES/sat-wasm.json)"

$K delete wasmplugin model-router-perf --wait=false >/dev/null 2>&1
$K apply -f /tmp/perf-manifests/envoyfilter-perf.yaml >/dev/null
verify 1 || exit 1
$LT -concurrency 64 -seconds 20 -warmup 5 > $RES/sat-ext2.json
echo "ext2: $(grep -o '"rps": [0-9.]*' $RES/sat-ext2.json)"
echo SAT-DONE
