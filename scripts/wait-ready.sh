#!/usr/bin/env bash
# Waits for every workload in the stack to roll out.
set -euo pipefail
TIMEOUT="${TIMEOUT:-300s}"
kubectl -n observability rollout status daemonset/otel-agent --timeout="$TIMEOUT"
for d in otel-gateway jaeger prometheus grafana slo-engine; do
  kubectl -n observability rollout status "deployment/$d" --timeout="$TIMEOUT"
done
for d in inventory payment catalog checkout frontend loadgen; do
  kubectl -n demo rollout status "deployment/$d" --timeout="$TIMEOUT"
done
echo "all workloads ready"
