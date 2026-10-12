# Architecture and design decisions

## Signal flow

1. **Instrumentation.** Each service uses the OTel Go SDK. `otelhttp` creates SERVER spans and the `http.server.request.duration` histogram. The instrumented transport creates CLIENT spans and injects `traceparent`. Pod identity (`k8s.pod.name`, `k8s.pod.uid`, `k8s.namespace.name`, `k8s.container.name`) comes from the Downward API through `OTEL_RESOURCE_ATTRIBUTES`.
2. **Agent (DaemonSet).** Receives OTLP on a node-local Service, so telemetry never leaves the node before enrichment. Adds Kubernetes metadata with `k8sattributes` (deployment, node, image, labels) and cloud metadata with `resourcedetection`. Collects kubelet stats for every container on the node.
3. **Gateway (Deployment).** Collects cluster state with `k8s_cluster`. Derives RED metrics (`spanmetrics`) and dependency edges (`servicegraph`) from traces. Exports traces to Jaeger and metrics to a Prometheus scrape endpoint, plus a managed backend in the cloud overlays.
4. **slo-engine.** Reads Prometheus and computes SLO, SLA, traffic and container views. It publishes its results back through OTLP as `slo_*` and `sla_*` metrics, so dashboards and alerting use one store.

## Decisions

| Decision | Why | Trade-off |
|---|---|---|
| Agent + gateway Collectors | The standard two-tier pattern. Enrichment happens close to the source, and expensive processing (connectors, export fan-out) is centralised. | Two configs to own |
| Node-local Service instead of hostPort | Works on every CNI and needs no host networking, so pods can stay under the `restricted` Pod Security level | Needs Kubernetes 1.26+ (`internalTrafficPolicy: Local`) |
| SLIs from HTTP semantic-convention metrics | Exact counts, no sampling bias, language-agnostic | Services must be instrumented, which is the point |
| Service graph from traces | Shows real call paths without a service mesh or sidecars | Needs both spans of a pair at one gateway (see scale-out) and is sensitive to sampling |
| Multi-window multi-burn-rate alerts | High precision and recall with fast reset (SRE Workbook, ch. 5) | Needs 4 to 8 recording rules per SLO |
| SLA as a looser target on the same SLI | One source of truth, and the gap between SLO and SLA becomes the early-warning buffer | Contract windows such as calendar months may differ from rolling windows. Set `window` to match the contract. |
| Generated Prometheus rules | Paging doesn't depend on the engine being up | Rules have to be regenerated when the config changes (CI enforces this) |
| Collector config merge (`--config a --config b`) | Each cloud overlay is about 10 lines instead of a forked config | Lists are replaced, not merged, so overlays must restate pipeline lists |
| Plain Prometheus HTTP API client | Works with any compatible backend | Instant queries only (by design) |

## Production scale-out

The minikube layout runs one gateway replica. That's correct for `servicegraph` and `k8s_cluster`, but it's also a single point of failure. For production:

1. **Split cluster-scoped receivers.** Run `k8s_cluster` in its own single-replica Deployment (or use leader election with `k8s_leader_elector`) so the gateway can scale.
2. **Route traces by trace ID.** Put the `loadbalancing` exporter (`routing_key: traceID`, DNS or k8s resolver) in the agent, pointing at a headless Service for a gateway StatefulSet. Every span of a trace lands on the same gateway replica, which `servicegraph` and tail sampling both need.
3. **Tail sampling after the connectors.** Compute `spanmetrics` and `servicegraph` on 100% of spans, then apply `tail_sampling` only on the pipeline that exports to the trace backend. Your SLIs and graph stay exact while storage costs drop.
4. **HPA on the gateway**, driven by `otelcol_exporter_queue_size` and CPU, plus a PodDisruptionBudget.
5. **Alertmanager** routes `severity=page` to on-call and `severity=ticket` to the backlog. The `contract=sla` label sends SLA alerts to account management as well.
6. **Retention.** Long SLO windows (30d) need at least 30 days of retention, or recording rules on a long-term store such as Thanos, Mimir, AMP or GMP.
7. **Security.** Turn on TLS/mTLS between agent and gateway (`configtls`), apply NetworkPolicies so only the agent can reach the gateway, use SSO for Grafana (the lab overlay uses anonymous admin, local only), and sign images (the release workflow already emits SBOM and provenance).

## Cardinality guardrails

- Span names and the `http.route` attribute use route patterns (`GET /api/work`), never raw URLs.
- `spanmetrics` dimensions are limited to method, status code and namespace.
- `servicegraph` adds only `k8s.namespace.name`.
- `resource_to_telemetry_conversion` turns resource attributes into labels, which suits a lab. In large fleets, drop high-churn attributes (for example `k8s.pod.uid` and `host.id`) with a `transform` or `resource` processor before export.
