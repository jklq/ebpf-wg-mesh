# 7 — Prove paid and stateful GA

Production readiness is an exercised property. Topology (2.13), fault suite (3.17), and load limits (3.18) already sit next to the features they prove. This file is the remaining paid-GA and disaster-recovery bar.

Do not treat this file as a reason to delay 1.x or 2.x. Isolation, secret, domain, and sandbox tests belong on those items as they land.

## 7.1 Security checks in CI

Was: 9.5
Status: open
Depends on: none. Not a substitute for adversarial tests on 1.8, 1.2, 2.4b, 2.15, and 2.8.

Prompt:

```text
Add dependency, container-image, Go, TypeScript, protobuf, IaC, and eBPF scanning in CI with a documented triage policy. Paid GA additionally requires an independent penetration test and remediation review; scanners do not replace it. Do not write a fourteen-boundary threat-model encyclopedia in this item.
```

## 7.2 SLOs and incident response

Was: 9.6
Status: open
Depends on: 3.5, 3.9, 3.10

Prompt:

```text
Define measurable service indicators and initial SLOs for control API availability, deployment queue/start success, healthy ingress availability, log and metrics ingestion delay, build completion, and stateless recovery. Source indicators from VictoriaMetrics and authoritative control-plane state, exclude only documented maintenance, and add multi-window burn-rate alerts with clear ownership. Create concise runbooks for every paging alert, an incident command and severity process, customer-impact assessment, status-page updates, security escalation, and blameless post-incident review. Integrate an external status-page and paging provider through narrow interfaces rather than building them here. Run game days for control-plane loss, region/agent loss, bad deploy, and credential compromise. Add volume and billing indicators only after those products exist.
```

## 7.3 Version skew and cutover

Was: 9.7
Status: open
Depends on: none. This repo does not do rolling backward-compatible migrations yet.

Prompt:

```text
Agents and builders report version and capabilities; the control plane rejects unsupported combinations with actionable status rather than corrupting desired state. Database and protobuf cutovers are explicit convert-or-refuse steps with a backup, not dual-shape compatibility windows. Exercise eBPF program replacement without dropping fail-closed policy. Do not require one-version-back wire compatibility or SBOM-signed release trains in this item.
```

## 7.5 Continuous backup and disaster-recovery drills

Was: 9.4
Status: open
Depends on: 3.11. Include durable volumes only after 6.3.

Prompt:

```text
Automate scheduled recovery exercises rather than treating backup creation as success. Restore CockroachDB, object metadata/source archives, key-provider references, registry trust, and representative durable volumes into an isolated environment. Verify project membership, secrets decryption, source integrity, deployment and audit history, image availability, domain intent without publishing production DNS, metrics billing watermarks, backup catalog, and application-level volume sentinels. Measure achieved RPO/RTO, retain a signed result, alert on missed or failed drills, and prevent the recovery environment from contacting production agents, ingress, billing, notifications, or webhooks. Include a documented regional-loss/manual bootstrap procedure and identify every external dependency the operator must restore first.
```
