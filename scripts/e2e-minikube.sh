#!/usr/bin/env bash
# End-to-end test of the whole pipeline on the current kube context
# (minikube locally, minikube in GitHub Actions). Requires kubectl, curl, jq.
#
#   1. traces flow app -> agent -> gateway -> Jaeger
#   2. servicegraph edges appear for the real call topology
#   3. SLOs evaluate with data; container-level metrics are joined
#   4. fault injection makes the payment SLO page and breach its SLA
set -euo pipefail
OUT="${OUT:-e2e-out}"
mkdir -p "$OUT"
pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

pf() { kubectl -n "$1" port-forward "svc/$2" "$3" >/dev/null 2>&1 & pids+=($!); }
pf observability slo-engine 18081:8081
pf observability prometheus 19090:9090
pf observability jaeger 16686:16686
sleep 3

API=http://localhost:18081
PROM=http://localhost:19090
FAILED=0
: > "$OUT/results.txt"
pass() { echo "PASS: $*" | tee -a "$OUT/results.txt"; }
fail() { echo "FAIL: $*" | tee -a "$OUT/results.txt"; FAILED=$((FAILED+1)); }

# retry <seconds> <description> <command...>  (records failure, keeps going)
retry() {
  local deadline=$(( $(date +%s) + $1 )); local what=$2; shift 2
  until "$@"; do
    if [ "$(date +%s)" -ge "$deadline" ]; then fail "$what (timed out)"; return 0; fi
    sleep 10
  done
  pass "$what"
}
snapshot() {
  curl -fs "$PROM/api/v1/label/__name__/values" | jq -r '.data[]' > "$OUT/metric-names.txt" || true
  curl -fs "$API/api/v1/slos" > "$OUT/slos.json" || true
  curl -fs "$API/api/v1/traffic" > "$OUT/traffic.json" || true
  curl -fs "$API/api/v1/containers" > "$OUT/containers.json" || true
  curl -fs "$API/api/v1/summary" > "$OUT/summary.json" || true
  curl -fs "http://localhost:16686/api/services" > "$OUT/jaeger-services.json" || true
  curl -fs "$PROM/api/v1/targets" | jq '[.data.activeTargets[] | {job: .labels.job, health, lastError}]' > "$OUT/prom-targets.json" || true
}

has_edge() { curl -fs "$API/api/v1/traffic" | jq -e --arg c "$1" --arg s "$2" '.edges[] | select(.client==$c and .server==$s and .rps > 0)' >/dev/null; }
slo_has_data() { curl -fs "$API/api/v1/slos/$1" | jq -e '.has_data == true' >/dev/null; }
jaeger_has() { curl -fs "http://localhost:16686/api/services" | jq -e --arg s "$1" '.data | index($s)' >/dev/null; }
prom_has() { curl -fs "$PROM/api/v1/query" --data-urlencode "query=$1" | jq -e '.data.result | length > 0' >/dev/null; }

echo "--- traces"
retry 300 "Jaeger received traces from payment" jaeger_has payment

echo "--- service graph"
for e in "loadgen frontend" "frontend catalog" "frontend checkout" "checkout payment" "checkout inventory"; do
  # shellcheck disable=SC2086
  retry 120 "edge ${e/ / -> }" has_edge $e
done
curl -fs "$API/api/v1/traffic/mermaid" | tee "$OUT/traffic.mmd"

echo "--- SLO evaluation"
for s in frontend-availability frontend-latency checkout-availability payment-availability catalog-latency checkout-to-payment-edge; do
  retry 90 "SLO $s has data" slo_has_data "$s"
done

echo "--- container-level metrics"
retry 120 "kubeletstats container CPU for demo pods" prom_has 'container_cpu_usage{k8s_namespace_name="demo"}'
retry 60 "container memory working set" prom_has 'container_memory_working_set_bytes{k8s_namespace_name="demo"}'
retry 60 "CPU limit utilization" prom_has 'k8s_container_cpu_limit_utilization_ratio{k8s_namespace_name="demo"}'
retry 60 "k8s_cluster container restarts" prom_has 'k8s_container_restarts{k8s_namespace_name="demo"}'
retry 60 "RED metrics carry k8s pod labels" prom_has 'http_server_request_duration_seconds_count{k8s_namespace_name="demo",k8s_pod_name=~"payment-.*"}'
snapshot
jq -e '[.[] | select(.pod | startswith("payment")) | select(.cpu_cores > 0 and .request_rate > 0)] | length >= 1' "$OUT/containers.json" >/dev/null \
  && pass "container view joins resource + RED metrics" || fail "container join"

echo "--- engine self-metrics through OTel"
retry 60 "slo_sli exported via OTLP" prom_has 'slo_sli{slo="frontend-availability"}'

echo "--- fault injection"
./scripts/chaos.sh inject
page_firing() { curl -fs "$API/api/v1/slos/payment-availability" | jq -e '[.alerts[] | select(.severity=="page" and .firing)] | length > 0' >/dev/null; }
retry 300 "payment-availability pages after 30% errors" page_firing
sla_breached() { curl -fs "$API/api/v1/sla" | jq -e '.[] | select(.name=="payment-availability") | .sla.state=="BREACHED"' >/dev/null; }
retry 120 "payment SLA reported BREACHED with credits" sla_breached
hot_edge() { curl -fs "$API/api/v1/summary" | jq -e '.hot_edges[] | select(.client=="checkout" and .server=="payment")' >/dev/null; }
retry 120 "checkout -> payment flagged as hot edge" hot_edge
./scripts/chaos.sh heal

snapshot
jq . "$OUT/summary.json" || true
echo "--- results"; cat "$OUT/results.txt"
[ "$FAILED" -eq 0 ] || { echo "E2E FAILED: $FAILED check(s)"; exit 1; }
echo "E2E OK"
