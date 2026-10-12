#!/usr/bin/env bash
# Opens local tunnels to the UIs. Ctrl-C to stop.
set -euo pipefail
trap 'kill $(jobs -p) 2>/dev/null' EXIT
kubectl -n observability port-forward svc/grafana 3000:3000 >/dev/null &
kubectl -n observability port-forward svc/prometheus 9090:9090 >/dev/null &
kubectl -n observability port-forward svc/jaeger 16686:16686 >/dev/null &
kubectl -n observability port-forward svc/slo-engine 8081:8081 >/dev/null &
cat <<MSG
Grafana     http://localhost:3000   (dashboard: SLO, SLA and Service Traffic)
Prometheus  http://localhost:9090/alerts
Jaeger      http://localhost:16686
SLO API     http://localhost:8081/api/v1/summary
            http://localhost:8081/api/v1/traffic/mermaid
MSG
wait
