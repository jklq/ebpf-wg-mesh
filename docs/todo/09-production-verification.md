# Stage 9 — Production Verification and Release Discipline

Production readiness is an exercised property. These tasks prove the preceding capabilities under realistic failure and load.

## 9.1 Build a repeatable production-like topology

Prompt:

```text
Extend the OpenTofu/VM harness into a production-like disposable topology with multiple control-plane replicas, a CockroachDB cluster, VictoriaMetrics, ClickHouse, object storage, registry, at least two ingress instances, builders, and at least three agents across distinct failure domains. Keep external managed-provider behaviors behind test doubles or lightweight compatible services where provisioning the real provider is inappropriate. Generate per-run credentials, expose no admin service publicly, collect sanitized artifacts, and guarantee teardown on success, failure, or interruption. The harness must deploy the actual built binaries and configuration profiles rather than alternate test implementations. Produce a machine-readable topology manifest and health summary so scenario tests can target components deterministically.
```

## 9.2 Add failure and recovery scenario tests

Prompt:

```text
Create a focused fault suite against the production-like topology. Kill and restart control-plane replicas, Cockroach nodes, agents, ingress instances, builders, VictoriaMetrics, ClickHouse, object storage, registry, and network links at meaningful transition points. Verify that active healthy workloads stay reachable within the stated availability target, old deployment generations remain serving during failed rollouts, stateless replicas reschedule, stateful workloads never gain two writers, durable work resumes without duplicate effects, logs/metrics report bounded gaps honestly, and queued actions do not disappear. Include clock skew and expired credentials where feasible. Record recovery time and invariant failures as artifacts. Keep scenarios few and high-value rather than creating a combinatorial chaos framework.
```

## 9.3 Establish load, scale, and noisy-neighbor limits

Prompt:

```text
Define initial supported scale targets for workspaces, projects, environments, services, allocations, agents, concurrent builds, desired-state snapshot size, domains, log lines per second, VictoriaMetrics samples/cardinality, API requests, and ingress connections. Build reproducible load tests that exercise control-plane reads and mutations, scheduler placement, agent reconnect storms, rollout fan-out, log/metrics ingestion, deployment history, billing rollups, and console-critical queries at those targets. Measure latency percentiles, error rate, Cockroach contention, queue age, memory, CPU, and recovery after load. Include one intentionally noisy tenant and prove quotas protect others. Turn discovered cliffs into enforced product limits or engineering fixes; publish the limits and fail admission before undefined behavior.
```

## 9.4 Verify backup and disaster recovery continuously

Prompt:

```text
Automate scheduled recovery exercises rather than treating backup creation as success. Restore CockroachDB, object metadata/source archives, key-provider references, registry trust, and representative durable volumes into an isolated environment. Verify project membership, secrets decryption, source integrity, deployment and audit history, image availability, domain intent without publishing production DNS, metrics billing watermarks, backup catalog, and application-level volume sentinels. Measure achieved RPO/RTO, retain a signed result, alert on missed or failed drills, and prevent the recovery environment from contacting production agents, ingress, billing, notifications, or webhooks. Include a documented regional-loss/manual bootstrap procedure and identify every external dependency the operator must restore first.
```

## 9.5 Add security verification and threat-model gates

Prompt:

```text
Write and maintain threat models for tenant isolation, control-plane compromise, agent compromise, hostile containers, hostile builds, source-provider compromise, registry tampering, domain takeover, secret handling, public API tokens, support access, billing fraud, and backup theft. Turn the highest-risk boundaries into executable checks: authorization matrices, cross-environment network denial on both families, container/build sandbox escapes, SSRF, archive traversal, command injection, log/header injection, secret redaction, token scope and revocation, image signature enforcement, stale attachment fencing, and domain reuse. Add dependency, container-image, Go, TypeScript, protobuf, IaC, and eBPF scanning in CI with a documented triage policy. Require an independent penetration test and remediation review before paid GA; do not replace it with automated scanners.
```

## 9.6 Define SLOs, incident response, and customer communication

Prompt:

```text
Define measurable service indicators and initial SLOs for control API availability, deployment queue/start success, healthy ingress availability, log and metrics ingestion delay, build completion, stateless recovery, volume operations, backup freshness, and billing accuracy. Source indicators from VictoriaMetrics and authoritative control-plane state, exclude only documented maintenance, and add multi-window burn-rate alerts with clear ownership. Create concise runbooks for every paging alert, an incident command and severity process, customer-impact assessment, status-page updates, security escalation, and blameless post-incident review. Integrate an external status-page and paging provider through narrow interfaces rather than building them here. Run game days for control-plane loss, region/agent loss, bad deploy, storage outage, metrics outage, and credential compromise.
```

## 9.7 Create release, migration, and rollback discipline

Prompt:

```text
Define how control plane, console, agents, builders, protobufs, database migrations, eBPF programs, and ingress configuration are versioned and rolled out safely. Database migrations must be backward-compatible across the supported rolling-upgrade window, separately observable, and irreversible only with an explicit backup and release note. Agents and builders report version/capabilities; the control plane rejects unsupported combinations with actionable status rather than corrupting desired state. Add canary rollout, health gates, automatic halt, operator rollback, signed release artifacts, SBOM/provenance, and upgrade notes. Exercise one-version-forward and one-version-back compatibility in CI/VM tests, including control-plane restart during migration and eBPF program replacement without dropping policy fail-closed guarantees.
```

## 9.8 Publish support boundaries and production readiness checks

Prompt:

```text
Create a machine-checkable production readiness report for an installation and a human checklist for a project. Installation checks must cover replica count, dependency durability, TLS and key provider, backup freshness and restore drill, ingress convergence, capacity headroom, VictoriaMetrics/ClickHouse retention, object storage, registry trust, on-call routes, supported versions, and unsafe development flags. Project checks must cover replicas, health probes, restart policy, resource limits, graceful shutdown, domains, monitors, spend limits, backups for attached volumes, and recent successful deployment. Clearly document unsupported behavior and failure domains at each release gate. The console may warn or block high-risk production promotion according to operator policy, but it must never imply high availability merely because a service is currently healthy.
```
