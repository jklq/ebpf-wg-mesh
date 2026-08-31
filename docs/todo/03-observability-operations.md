# Stage 3 — Observability and Operations

This stage makes the platform and its workloads diagnosable. VictoriaMetrics owns resource time series, ClickHouse owns logs and high-cardinality events, and CockroachDB owns durable product state and billing rollups.

## 3.1 Collect trustworthy workload and platform resource metrics

Prompt:

```text
Add a metrics path from agents, builders, ingress, and control-plane components into VictoriaMetrics. Collect allocation CPU time, throttling, working-set and RSS memory, OOM events, network ingress/egress bytes, ephemeral disk usage, persistent volume usage when available, restart counts, probe state, and allocation uptime from authoritative kernel/runtime counters. Collect build CPU, memory, duration, queue time, and transferred bytes separately. Tag samples with stable workspace/project/environment/service/allocation/build/agent identifiers and region or failure-domain labels, but never user-controlled secret values or unbounded log content. Define counter reset, allocation replacement, clock skew, scrape/remote-write retry, duplicate sample, and temporary-disconnection semantics. Metrics delivery must be buffered and bounded so an unavailable VictoriaMetrics cluster cannot exhaust an agent. Provide dashboards and tests proving aggregation across replicas without double counting.
```

## 3.2 Add service and environment observability views

Prompt:

```text
Build console observability views backed by VictoriaMetrics for resource series and ClickHouse for logs/events. A service view should correlate deployments with CPU, memory, OOMs, restarts, network traffic, probe transitions, replica count, request volume, error rate, and latency where ingress can observe them. An environment view should aggregate services while allowing drill-down by allocation and deployment generation. Support fixed and custom time ranges, stable downsampling, timezone-aware labels, missing-data explanation, and links from a chart anomaly to the relevant deployment and filtered logs. Query authorization must be enforced server-side from membership, not only by UI filtering. Bound query range, cardinality, and response size to protect shared backends, and test that one project cannot infer another project’s series or labels.
```

## 3.3 Make logs durable, bounded, and useful across replicas

Prompt:

```text
Harden the existing ClickHouse log path for multi-tenant production use. Preserve runtime, build, deploy, HTTP, and network log types; add structured attributes for known platform events without parsing arbitrary customer output; define ordering and duplicate handling across reconnects; and retain the raw line exactly within a documented size limit. Agents and builders need bounded disk-backed spooling, batching, backpressure, retry, and explicit dropped-line counters so a ClickHouse outage cannot consume unbounded memory or block workload reconciliation. Enforce per-allocation rate and burst limits, tenant retention policies, authorized time-range search, pagination or streaming, and safe deletion after project expiry. The console should offer environment-wide search and deployment-scoped logs with clear gaps when data was dropped. Test backend outage, retry duplication, oversized lines, abusive log rates, retention, and tenant isolation.
```

## 3.4 Add monitors, notifications, and signed webhooks

Prompt:

```text
Create persisted monitors for deployment failure, crash loop, no healthy replica, CPU saturation, memory/OOM pressure, volume capacity, build queue delay, build failure, agent loss, ingress sync failure, log loss, metrics ingestion lag, backup staleness, and billing-limit approach. Evaluate metric monitors from VictoriaMetrics and state/event monitors from authoritative control-plane data, with configurable duration, threshold, severity, deduplication key, cooldown, and resolved notifications. Deliver in-app and email notifications through provider interfaces, plus project webhooks signed with a rotating secret. Webhook delivery needs an outbox, attempts, exponential retry, terminal failure visibility, replay, SSRF-safe URL validation, and no secret-bearing payloads. Give users test controls and event filters. Add deterministic rule tests and integration tests for firing, deduplication, resolution, retry, replay, and unauthorized configuration.
```

## 3.5 Add an operator control room and diagnostic bundles

Prompt:

```text
Create an operator-only view and API that summarize control-plane replica health, CockroachDB and VictoriaMetrics reachability, ClickHouse ingestion, object storage, registry auth, builder capacity, build queue age, agent heartbeat age, allocation capacity, ingress convergence, certificate expiry, backup freshness, notification failures, and garbage-collection backlog. Every degraded item must identify the affected scope and link to a runbook without exposing customer secrets. Add a diagnostic bundle command that gathers sanitized configuration shape, component versions, recent platform events, relevant metrics summaries, and bounded logs for a selected time and resource scope; bundles must be inspectable before export and carry an expiry. Define severity and ownership for each signal so the page is actionable instead of a wall of green checks. Exercise degraded dependencies in the local/VM harness.
```
