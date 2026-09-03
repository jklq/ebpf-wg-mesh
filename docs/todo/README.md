# Production Platform Backlog

This directory turns platform gaps into implementation prompts. Each prompt describes the product outcome and leaves the implementing agent free to choose a simple design that fits the repository.

Work is ordered by **what has to be true for this product to work**, not by subsystem. Do not finish a whole category before starting the next. Pick the next open item in the work queue whose real dependencies are met.

## How to choose work

1. Start at the top of the [work queue](#work-queue).
2. Skip only when a listed dependency is still open, or when a later item has no unmet dependency and unblocks a real user this week.
3. Implement that prompt end to end: protobuf and persisted model, authorization, control-plane behavior, agent/builder where relevant, console UX, configuration, tests, local-stack support, and operator documentation. If a prompt names several subsystems, the first merge is the primitive plus one caller, not a flag day across every reconciler.
4. Prefer a small coherent model over compatibility scaffolding. Cut over code freely. Persisted customer authority (secrets, addresses, domains) needs an explicit convert-or-refuse step, not a silent leftover shape.
5. When a prompt discovers a missing earlier invariant, fix it at the owning layer instead of adding a console-only workaround.

Files are slices of that queue, not sequential gates. Category is a tag, not a mutex.

## Architecture to preserve

- CockroachDB is the authoritative control-plane store. High-frequency probe ticks, logs, and metrics samples do not become Cockroach rows.
- Agents stay dumb. The control plane is authoritative for placement. Each agent is sent only the allocations assigned to that agent, as Nomad sends clients only their allocs: diffs to start, update, or stop, plus a reconnect reconcile of “what I am running” against “what this node should run.” A full cluster snapshot is not the wire format and not the recovery format.
- Policy is fail-closed and identity-based. An agent receives a pool deny plus exact allows for environments it currently hosts (including remote allocations in those environments). It does not receive a cluster-wide identity catalog. Ingress proxies receive the backends they route, not the mesh catalog.
- WireGuard is the overlay transport and eBPF is the identity policy. There is no full mesh. Peers exist only between nodes that share an environment and between those nodes and the Envoy instances that publish their services. Unused peers are removed.
- containerd remains the workload runtime.
- Envoy is the ingress data plane. The control plane serves a versioned xDS snapshot; Envoy ACK/NACK is the apply protocol. Do not keep Caddy as a production ingress. Do not implement Envoy, a CDN, or an edge network in this repository.
- ClickHouse is the log store. It is not a metrics system and not a billing meter.
- VictoriaMetrics holds resource time series as evidence. CockroachDB holds immutable billing rollups and ledger state. A usage bucket that cannot be explained against allocation/build/volume lifecycle intervals is wrong.
- Immutable source snapshots and deploy-by-digest are the source/runtime trust model. GitHub App grants are the current way to produce snapshots, not an eternal vendor lock-in of identity and source.
- Control-plane replicas have no node-local authority: no shared filesystem of keys or source archives as the production contract.
- In-process PKI signs agent mTLS and registry tokens. That is not a certificate-authority product. Sign and unwrap through a key provider; do not invent customer-facing CA ceremony.
- Production integrations (object storage, KMS, billing, durable block storage, email) use narrow provider interfaces. Do not implement a new distributed database, workflow engine, storage engine, or global edge network here.
- Overlay dual-stack (1.1) does not change the underlay. Agent `advertise_addr` stays IPv6 until an explicit underlay prompt exists; IPv4-only hosts cannot join the mesh.

## Product gates

Gates are about a running product, not about emptying a folder.

### Dogfood

A known person can connect a typical GitHub repository, set secrets, deploy, and reach a process that bound `0.0.0.0` or `::`. Status, health, crashes, and timestamps are truthful. Auto-deploy can be turned off. Delete does not instantly destroy recoverable resources.

Requires the open items in [01-running-service.md](01-running-service.md).

### Design-partner beta

That loop survives hosting someone else’s code: isolated builds, digest-pinned images, durable source and keys, an Envoy fleet, real domains/certs, logs that outlive a ClickHouse blip, Nomad-style node sync, scoped identity, environment-scoped peering, and a topology you can fail. No important persistent customer data on platform volumes. No GA promise.

Requires [01-running-service.md](01-running-service.md) and [02-host-untrusted-code.md](02-host-untrusted-code.md).

### Multi-tenant stateless

More than one team can share the installation with RBAC, quotas, audit, metrics, explicit HTTP exposure, and an API/CLI. Platform backup exists.

Requires [03-operate-multi-tenant.md](03-operate-multi-tenant.md).

### Paid stateless

Plans, metering, spend limits, abuse controls, support access, and the GA proof in [07-ga-proof.md](07-ga-proof.md). Developer-surface items in [04-developer-surface.md](04-developer-surface.md) may land earlier whenever they help.

### Stateful production

Advertise databases or durable customer workloads only after [06-stateful.md](06-stateful.md) and the recovery drills in 7.5. A local bind mount is not a production volume.

## Work queue

Landed work is recorded in [landed.md](landed.md). Old `x.y` IDs are in parentheses.

### Next: the running service is real

| # | Item | File |
| --- | --- | --- |
| 1.1 | Dual-stack overlay so `0.0.0.0` apps work (5.1) | [01](01-running-service.md#11-dual-stack-workload-overlay) |
| 1.2 | Encrypted versioned secrets (2.4) | [01](01-running-service.md#12-encrypted-versioned-secrets) |
| 1.3 | Continuous readiness and liveness (1.2) | [01](01-running-service.md#13-continuous-readiness-and-liveness) |
| 1.4 | Crash evidence in the console | [01](01-running-service.md#14-crash-evidence-in-the-console) |
| 1.5 | Truthful time and status (0.2) | [01](01-running-service.md#15-truthful-time-and-status) |
| 1.6 | Railpack as the default builder (4.1) | [01](01-running-service.md#16-railpack-as-the-default-builder) |
| 1.7 | Auto-deploy on/off per environment | [01](01-running-service.md#17-auto-deploy-onoff-per-environment) |
| 1.8 | Safe deletion (0.3) | [01](01-running-service.md#18-safe-deletion) |
| 1.9 | Credentials out of process arguments (0.1) | [01](01-running-service.md#19-credentials-out-of-process-arguments) |
| 1.10 | Builder and agent architecture matching | [01](01-running-service.md#110-builder-and-agent-architecture-matching) |
| 1.11 | Bounded ephemeral disk | [01](01-running-service.md#111-bounded-ephemeral-disk) |

### Then: host untrusted code without a shared disk

| # | Item | File |
| --- | --- | --- |
| 2.1 | Durable-work package (2.7) | [02](02-host-untrusted-code.md#21-durable-work-package) |
| 2.2 | Source object storage (2.2) | [02](02-host-untrusted-code.md#22-source-object-storage) |
| 2.3 | Externalize platform keys (2.3) | [02](02-host-untrusted-code.md#23-externalize-platform-keys) |
| 2.4 | Isolate untrusted builds (4.2) | [02](02-host-untrusted-code.md#24-isolate-untrusted-builds) |
| 2.5 | Build leases, cancellation, fairness (4.3) | [02](02-host-untrusted-code.md#25-build-leases-cancellation-and-fairness) |
| 2.6 | Deploy-by-digest (4.5) | [02](02-host-untrusted-code.md#26-deploy-by-digest) |
| 2.7 | Envoy ingress fleet | [02](02-host-untrusted-code.md#27-envoy-ingress-fleet) |
| 2.8 | Domain and certificate lifecycle (5.3) | [02](02-host-untrusted-code.md#28-domain-and-certificate-lifecycle) |
| 2.9 | Durable bounded logs (3.3) | [02](02-host-untrusted-code.md#29-durable-bounded-logs) |
| 2.10 | Nomad-style per-node allocation sync | [02](02-host-untrusted-code.md#210-nomad-style-per-node-allocation-sync) |
| 2.11 | Scoped identity policy | [02](02-host-untrusted-code.md#211-scoped-identity-policy) |
| 2.12 | Environment-scoped WireGuard peering | [02](02-host-untrusted-code.md#212-environment-scoped-wireguard-peering) |
| 2.13 | Production-like topology harness (9.1) | [02](02-host-untrusted-code.md#213-production-like-topology-harness) |
| 2.14 | Core onboarding path (8.9) | [02](02-host-untrusted-code.md#214-core-onboarding-path) |

### Then: more than one team on one installation

| # | Item | File |
| --- | --- | --- |
| 3.1 | Organizations and RBAC (6.1) | [03](03-operate-multi-tenant.md#31-organizations-and-rbac) |
| 3.2 | Scoped API credentials and sessions (6.3) | [03](03-operate-multi-tenant.md#32-scoped-api-credentials-and-sessions) |
| 3.3 | Resource quotas (6.4) | [03](03-operate-multi-tenant.md#33-resource-quotas) |
| 3.4 | Audit history (6.2) | [03](03-operate-multi-tenant.md#34-audit-history) |
| 3.5 | Workload and platform metrics (3.1) | [03](03-operate-multi-tenant.md#35-workload-and-platform-metrics) |
| 3.6 | Service and environment observability views (3.2) | [03](03-operate-multi-tenant.md#36-service-and-environment-observability-views) |
| 3.7 | Explicit HTTP exposure (5.4) | [03](03-operate-multi-tenant.md#37-explicit-http-exposure) |
| 3.8 | Egress policy (5.5) | [03](03-operate-multi-tenant.md#38-egress-policy) |
| 3.9 | Monitors, notifications, webhooks (3.4) | [03](03-operate-multi-tenant.md#39-monitors-notifications-and-webhooks) |
| 3.10 | Operator control room (3.5) | [03](03-operate-multi-tenant.md#310-operator-control-room) |
| 3.11 | Platform backup and restore (2.5) | [03](03-operate-multi-tenant.md#311-platform-backup-and-restore) |
| 3.12 | Public customer API (8.1) | [03](03-operate-multi-tenant.md#312-public-customer-api) |
| 3.13 | Platform CLI (8.2) | [03](03-operate-multi-tenant.md#313-platform-cli) |
| 3.14 | Tenant-safe build cache (4.4) | [03](03-operate-multi-tenant.md#314-tenant-safe-build-cache) |
| 3.15 | Image and artifact lifecycle (4.6) | [03](03-operate-multi-tenant.md#315-image-and-artifact-lifecycle) |
| 3.16 | Production registry contract (4.7) | [03](03-operate-multi-tenant.md#316-production-registry-contract) |
| 3.17 | Failure and recovery scenarios (9.2) | [03](03-operate-multi-tenant.md#317-failure-and-recovery-scenarios) |
| 3.18 | Load, scale, and noisy-neighbor limits (9.3) | [03](03-operate-multi-tenant.md#318-load-scale-and-noisy-neighbor-limits) |

### Convenience on a working substrate

Do not postpone 1.x or 2.x for these.

| # | Item | File |
| --- | --- | --- |
| 4.1 | Repository configuration as code (8.3) | [04](04-developer-surface.md#41-repository-configuration-as-code) |
| 4.2 | Reference variables (8.7) | [04](04-developer-surface.md#42-reference-variables) |
| 4.3 | Monorepo watch paths (8.5) | [04](04-developer-surface.md#43-monorepo-watch-paths) |
| 4.4 | Pre-deploy jobs and environment deploy DAG (8.6) | [04](04-developer-surface.md#44-pre-deploy-jobs-and-environment-deploy-dag) |
| 4.5 | Pull-request environments (8.4) | [04](04-developer-surface.md#45-pull-request-environments) |

### Charge and govern

| # | Item | File |
| --- | --- | --- |
| 5.1 | VictoriaMetrics billing meter (6.5) | [05](05-commercial.md#51-victoriametrics-billing-meter) |
| 5.2 | Plans, credits, and spend controls (6.6) | [05](05-commercial.md#52-plans-credits-and-spend-controls) |
| 5.3 | Fair-use and abuse safeguards (6.7) | [05](05-commercial.md#53-fair-use-and-abuse-safeguards) |
| 5.4 | Account export, retention, and closure (6.8) | [05](05-commercial.md#54-account-export-retention-and-closure) |
| 5.5 | Operator and support access (6.9) | [05](05-commercial.md#55-operator-and-support-access) |
| 7.1 | Security checks in CI (9.5) | [07](07-ga-proof.md#71-security-checks-in-ci) |
| 7.2 | SLOs and incident response (9.6) | [07](07-ga-proof.md#72-slos-and-incident-response) |
| 7.3 | Version skew and cutover (9.7) | [07](07-ga-proof.md#73-version-skew-and-cutover) |
| 7.4 | Support boundaries and readiness checks (9.8) | [07](07-ga-proof.md#74-support-boundaries-and-readiness-checks) |

### Last: stateful production

| # | Item | File |
| --- | --- | --- |
| 6.1 | Volume provider contract (7.1) | [06](06-stateful.md#61-volume-provider-contract) |
| 6.2 | Attachment, mount paths, and resize (7.2) | [06](06-stateful.md#62-attachment-mount-paths-and-resize) |
| 6.3 | Volume snapshots, backups, and restores (7.3) | [06](06-stateful.md#63-volume-snapshots-backups-and-restores) |
| 6.4 | Fenced stateful failover (7.4) | [06](06-stateful.md#64-fenced-stateful-failover) |
| 6.5 | Storage monitoring and safety rails (7.5) | [06](06-stateful.md#65-storage-monitoring-and-safety-rails) |
| 7.5 | Continuous backup and disaster-recovery drills (9.4) | [07](07-ga-proof.md#75-continuous-backup-and-disaster-recovery-drills) |
