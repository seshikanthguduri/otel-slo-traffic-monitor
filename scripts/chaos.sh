#!/usr/bin/env bash
# Fault injection through each service's /admin/fault endpoint.
#   chaos.sh inject  -> payment returns 30% 5xx, catalog gets a 20% slow tail
#   chaos.sh heal    -> back to baseline
set -euo pipefail
mode="${1:-inject}"
call() { # svc query
  kubectl -n demo get pods -l "app.kubernetes.io/name=$1" -o name | while read -r pod; do
    kubectl -n demo port-forward "$pod" 18080:8080 >/dev/null 2>&1 & pf=$!
    for _ in $(seq 1 30); do curl -fs localhost:18080/healthz >/dev/null 2>&1 && break; sleep 0.2; done
    printf '%s: ' "$pod"; curl -fsS -XPOST "localhost:18080/admin/fault?$2"; echo
    kill $pf; wait $pf 2>/dev/null || true
  done
}
case "$mode" in
  inject) call payment "error_rate=0.3"; call catalog "slow_rate=0.2&slow_latency_ms=900" ;;
  heal)   call payment "error_rate=0.002"; call catalog "slow_rate=0.02&slow_latency_ms=700" ;;
  *) echo "usage: $0 inject|heal"; exit 2 ;;
esac
