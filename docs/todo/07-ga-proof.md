# 7 — Prove paid and stateful GA

Production readiness is an exercised property. Topology (2.11), fault suite (3.17), and load limits (3.18) already sit next to the features they prove. This file is the remaining paid-GA and disaster-recovery bar.

Do not treat this file as a reason to delay 1.x or 2.x. Add executable checks to each earlier item as it lands; these prompts collect the installation-wide gates.

## 7.1 Security verification and threat models

Was: 9.5
Status: open
Depends on: the boundaries it names. Independent penetration test is a paid-GA gate, not a substitute for earlier isolation tests.

Prompt:

```text
Write and maintain threat models for tenant isolation, control-plane compromise, agent compromise, hostile containers, hostile builds, source-provider compromise, registry tampering, domain takeover, secret handling, public API tokens, support access, billing fraud, and backup theft. Turn the highest-risk boundaries into executable checks: authorization matrices, cross-environment network denial on both families, container/build sandbox escapes, SSRF, archive traversal, command injection, log/header injection, secret redaction, token scope and revocation, image signature enforcement, stale attachment fencing, and domain reuse. Add dependency, container-image, Go, TypeScript, protobuf, IaC, and eBPF scanning in CI with a documented triage policy. Require an independent penetration test and remediation review before paid GA; do not replace it with automated scanners.
```

## 7.2 SLOs and incident response

Was: 9.6
Status: open
Depends on: 3.5, 3.9, 3.10

Prompt:

```text
Define measurable service indicators and initial SLOs for control API availability, deployment queue/start success, healthy ingress availability, log and metrics ingestion delay, build completion, stateless recovery, volume operations, backup freshness, and billing accuracy. Source indicators from VictoriaMetrics and authoritative control-plane state, exclude only documented maintenance, and add multi-window burn-rate alerts with clear ownership. Create concise runbooks for every paging alert, an incident command and severity process, customer-impact assessment, status-page updates, security escalation, and blameless post-incident review. Integrate an external status-page and paging provider through narrow interfaces rather than building them here. Run game days for control-plane loss, region/agent loss, bad deploy, storage outage, metrics outage, and credential compromise.
```

## 7.3 Release, migration, and rollback discipline

Was: 9.7
Status: open
Depends on: 2.6 for signed artifacts where claimed

Prompt:

```text
Define how control plane, console, agents, builders, protobufs, database migrations, eBPF programs, and ingress configuration are versioned and rolled out safely. Database migrations must be backward-compatible across the supported rolling-upgrade window, separately observable, and irreversible only with an explicit backup and release note. Agents and builders report version/capabilities; the control plane rejects unsupported combinations with actionable status rather than corrupting desired state. Add canary rollout, health gates, automatic halt, operator rollback, signed release artifacts, SBOM/provenance, and upgrade notes. Exercise one-version-forward and one-version-back compatibility in CI/VM tests, including control-plane restart during migration and eBPF program replacement without dropping policy fail-closed guarantees.
```

## 7.4 Support boundaries and readiness checks

Was: 9.8
Status: open
Depends on: 3.11 for backup freshness. Volume and spend checks apply only when those products exist.

Prompt:

```text
Create a machine-checkable production readiness report for an installation and a human checklist for a project. Installation checks must cover replica count, dependency durability, TLS and key provider, backup freshness and restore drill, ingress convergence, capacity headroom, VictoriaMetrics/ClickHouse retention, object storage, registry trust, on-call routes, supported versions, and unsafe development flags. Project checks must cover replicas, health probes, restart policy, resource limits, graceful shutdown, domains, monitors, spend limits, backups for attached volumes, and recent successful deployment. Clearly document unsupported behavior and failure domains at each release gate. The console may warn or block high-risk production promotion according to operator policy, but it must never imply high availability merely because a service is currently healthy.
```

## 7.5 Continuous backup and disaster-recovery drills

Was: 9.4
Status: open
Depends on: 3.11. Include durable volumes only after 6.3.

Prompt:

```text
Automate scheduled recovery exercises rather than treating backup creation as success. Restore CockroachDB, object metadata/source archives, key-provider references, registry trust, and representative durable volumes into an isolated environment. Verify project membership, secrets decryption, source integrity, deployment and audit history, image availability, domain intent without publishing production DNS, metrics billing watermarks, backup catalog, and application-level volume sentinels. Measure achieved RPO/RTO, retain a signed result, alert on missed or failed drills, and prevent the recovery environment from contacting production agents, ingress, billing, notifications, or webhooks. Include a documented regional-loss/manual bootstrap procedure and identify every external dependency the operator must restore first.
```
