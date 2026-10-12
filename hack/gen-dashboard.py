#!/usr/bin/env python3
"""Generates deploy/base/grafana/dashboards/slo-overview.json (run: make dashboard)."""
import json, os

DS = {"type": "prometheus", "uid": "prometheus"}
panels, y = [], 0
pid = 0

def nid():
    global pid
    pid += 1
    return pid

def row(title):
    global y
    panels.append({"type": "row", "title": title, "id": nid(), "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": []})
    y += 1

def panel(kind, title, exprs, x, w, h=8, unit=None, desc="", extra=None, fmt=None):
    p = {"type": kind, "title": title, "id": nid(), "datasource": DS, "description": desc,
         "gridPos": {"h": h, "w": w, "x": x, "y": y},
         "targets": [dict({"refId": chr(65 + i), "datasource": DS, "expr": e, "legendFormat": l},
                          **({"format": fmt, "instant": True} if fmt else {}))
                     for i, (e, l) in enumerate(exprs)],
         "fieldConfig": {"defaults": {}, "overrides": []}, "options": {}}
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if extra:
        for k, v in extra.items():
            p[k] = v
    panels.append(p)

def advance(h=8):
    global y
    y += h

ns = '{k8s_namespace_name=~"$namespace"}'

row("SLO status (from slo-engine)")
panel("stat", "SLI over SLO window", [("slo_sli", "{{slo}}")], 0, 8, unit="percentunit",
      desc="good/valid events over the rolling SLO window",
      extra={"options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "background", "textMode": "value_and_name"},
             "fieldConfig": {"defaults": {"unit": "percentunit", "decimals": 3,
                 "thresholds": {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "orange", "value": 0.99}, {"color": "green", "value": 0.999}]}}, "overrides": []}})
panel("bargauge", "Error budget remaining", [("slo_error_budget_remaining", "{{slo}}")], 8, 8,
      desc="1 = untouched, 0 = exhausted, negative = overspent",
      extra={"options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "orientation": "horizontal", "displayMode": "gradient"},
             "fieldConfig": {"defaults": {"unit": "percentunit", "min": -1, "max": 1,
                 "thresholds": {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "orange", "value": 0.25}, {"color": "green", "value": 0.5}]}}, "overrides": []}})
panel("table", "Firing burn-rate alerts", [('slo_alert_firing == 1', "")], 16, 8, fmt="table",
      desc="Multi-window multi-burn-rate policies (SRE Workbook)",
      extra={"transformations": [{"id": "organize", "options": {"includeByName": {"slo": True, "severity": True, "long_window": True}}}]})
advance()
panel("timeseries", "Burn rate (fast windows)", [('slo_burn_rate{window=~"5m|1h"}', "{{slo}} {{window}}")], 0, 12,
      desc="1.0 = exactly on budget; 14.4 over 1h pages for a 30d window")
panel("table", "SLA compliance and service credits",
      [("sla_compliant", ""), ("sla_credit_percent", "")], 12, 12, fmt="table",
      extra={"transformations": [{"id": "merge"}, {"id": "organize", "options": {
          "includeByName": {"slo": True, "slo_service": True, "Value #A": True, "Value #B": True},
          "renameByName": {"Value #A": "compliant (1=yes)", "Value #B": "credit %"}}}]})
advance()

row("East-west traffic (OTel servicegraph connector)")
panel("table", "Service-to-service edges",
      [('sum by (client, server) (rate(traces_service_graph_request_total{server!="unknown"}[5m]))', ""),
       ('sum by (client, server) (rate(traces_service_graph_request_failed_total{server!="unknown"}[5m])) / sum by (client, server) (rate(traces_service_graph_request_total{server!="unknown"}[5m]))', ""),
       ('histogram_quantile(0.99, sum by (client, server, le) (rate(traces_service_graph_request_server_seconds_bucket{server!="unknown"}[5m])))', "")],
      0, 12, fmt="table",
      extra={"transformations": [{"id": "merge"}, {"id": "organize", "options": {
          "excludeByName": {"Time": True},
          "renameByName": {"Value #A": "req/s", "Value #B": "error ratio", "Value #C": "p99 (s)"}}}]})
panel("timeseries", "Edge error ratio", [('sum by (client, server) (rate(traces_service_graph_request_failed_total[5m])) / sum by (client, server) (rate(traces_service_graph_request_total[5m]))', "{{client}} -> {{server}}")], 12, 12, unit="percentunit")
advance()
panel("timeseries", "Edge p99 latency (server side)", [('histogram_quantile(0.99, sum by (client, server, le) (rate(traces_service_graph_request_server_seconds_bucket[5m])))', "{{client}} -> {{server}}")], 0, 12, unit="s")
panel("timeseries", "Edge throughput", [('sum by (client, server) (rate(traces_service_graph_request_total[5m]))', "{{client}} -> {{server}}")], 12, 12, unit="reqps")
advance()

row("Service RED (OTel HTTP semantic conventions)")
panel("timeseries", "Requests/s by service", [(f'sum by (service_name) (rate(http_server_request_duration_seconds_count{ns}[5m]))', "{{service_name}}")], 0, 8, unit="reqps")
panel("timeseries", "5xx ratio by service", [(f'sum by (service_name) (rate(http_server_request_duration_seconds_count{{k8s_namespace_name=~"$namespace",http_response_status_code=~"5.."}}[5m])) / sum by (service_name) (rate(http_server_request_duration_seconds_count{ns}[5m]))', "{{service_name}}")], 8, 8, unit="percentunit")
panel("timeseries", "p99 latency by service", [(f'histogram_quantile(0.99, sum by (service_name, le) (rate(http_server_request_duration_seconds_bucket{ns}[5m])))', "{{service_name}}")], 16, 8, unit="s")
advance()

row("Container level (kubeletstats + k8s_cluster receivers)")
panel("timeseries", "CPU usage by container", [(f'sum by (k8s_pod_name, k8s_container_name) (container_cpu_usage{ns})', "{{k8s_pod_name}}/{{k8s_container_name}}")], 0, 8, unit="short", desc="CPU cores")
panel("timeseries", "CPU % of limit", [(f'max by (k8s_pod_name, k8s_container_name) (k8s_container_cpu_limit_utilization_ratio{ns})', "{{k8s_pod_name}}/{{k8s_container_name}}")], 8, 8, unit="percentunit", desc="Sustained >80% means CPU throttling risk")
panel("timeseries", "Memory % of limit", [(f'max by (k8s_pod_name, k8s_container_name) (k8s_container_memory_limit_utilization_ratio{ns})', "{{k8s_pod_name}}/{{k8s_container_name}}")], 16, 8, unit="percentunit", desc="Approaching 100% means OOMKill risk")
advance()
panel("timeseries", "Memory working set by container", [(f'sum by (k8s_pod_name, k8s_container_name) (container_memory_working_set_bytes{ns})', "{{k8s_pod_name}}/{{k8s_container_name}}")], 0, 12, unit="bytes")
panel("timeseries", "Container restarts", [(f'max by (k8s_pod_name, k8s_container_name) (k8s_container_restarts{ns})', "{{k8s_pod_name}}/{{k8s_container_name}}")], 12, 12, unit="short")
advance()

dash = {
    "uid": "slo-overview", "title": "SLO, SLA and Service Traffic", "tags": ["slo", "sla", "opentelemetry"],
    "timezone": "browser", "schemaVersion": 39, "version": 1, "refresh": "30s",
    "time": {"from": "now-1h", "to": "now"},
    "templating": {"list": [{
        "name": "namespace", "type": "query", "datasource": DS, "label": "Namespace",
        "query": {"query": "label_values(http_server_request_duration_seconds_count, k8s_namespace_name)", "refId": "ns"},
        "definition": "label_values(http_server_request_duration_seconds_count, k8s_namespace_name)",
        "includeAll": True, "multi": True, "current": {"text": "All", "value": "$__all"}, "refresh": 2}]},
    "panels": panels,
}
out = os.path.join(os.path.dirname(__file__), "..", "deploy", "base", "grafana", "dashboards", "slo-overview.json")
with open(out, "w") as f:
    json.dump(dash, f, indent=2)
    f.write("\n")
print("wrote", os.path.normpath(out), len(panels), "panels")
