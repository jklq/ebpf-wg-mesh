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

**Numbers are names, not an order.** `2.6` does not have to follow `2.5`. The only real constraints are the `Depends on:` line on each item and the [product gates](#product-gates). Read the queue as a set with a dependency graph over it.

**Items are not the same size.** The `Size` column is a rough order of magnitude, not an estimate. `2.4b` is plausibly ten times `1.2`. Planning that treats the table as uniform units will be wrong by a lot.

**Test lists are the hidden cost.** Most prompts close with six to ten named scenarios. Summed across the backlog that is plausibly more work than the features. When implementing, split each prompt's list into the cases that must run in CI forever and the cases that are one-time proof — write both, keep the first, and record the second in the item's notes rather than the suite. A permanent test for every adversarial scenario in this directory is not affordable and not the point.

## Architecture to preserve

- CockroachDB is the authoritative control-plane store. High-frequency probe ticks, logs, metrics samples, and queue coordination do not become product-journal commands. The product journal boots from a consistent read of the normalized tables and retains a bounded replay tail; it does not serialize the complete product state into a checkpoint row. Command receipts have bounded retention: one hour for internal commit resolution and seven days for caller-provided idempotency.
- The control plane is authoritative for placement; an agent is authoritative for supervising work already assigned to it. Agents durably retain accepted allocation state and continue supervision through control-plane loss. Steady-state delivery is bounded per-node start, update, and stop diffs, plus a reconnect reconcile of “what I am running” against “what this node should run.” The current complete per-node snapshot is a safe foundation for that cutover, not the target wire or recovery format.
- Policy is fail-closed and identity-based. An agent receives a pool deny plus exact allows for environments it currently hosts (including remote allocations in those environments). It does not receive a cluster-wide identity catalog. Ingress proxies receive the backends they route, not the mesh catalog.
- The platform's own surface is unreachable from a customer container regardless of configuration: metadata addresses, host and management networks, control-plane and agent administration, registry credentials, and other tenants' overlays. That is a platform invariant (2.15), and no customer rule may relax it.
- WireGuard is the overlay transport and eBPF is the identity policy. There is no full mesh. Peers exist only between nodes that share an environment and between those nodes and the Envoy instances that publish their services. Unused peers are removed.
- containerd remains the workload runtime.
- Envoy is the ingress data plane. The control plane serves a versioned xDS snapshot; Envoy ACK/NACK is the apply protocol. Do not keep Caddy as a production ingress. Do not implement Envoy, a CDN, or an edge network in this repository.
- ClickHouse is the log store. It is not a metrics system and not a billing meter.
- VictoriaMetrics holds resource time series as evidence. CockroachDB holds immutable billing rollups and ledger state. A usage bucket that cannot be explained against allocation/build/volume lifecycle intervals is wrong.
- Immutable source snapshots and deploy-by-digest are the source/runtime trust model. GitHub App grants are the current way to produce snapshots, not an eternal vendor lock-in of identity and source.
- Control-plane replicas have no node-local authority: no shared filesystem of keys or source archives as the production contract.
- In-process PKI signs agent mTLS and registry tokens. That is not a certificate-authority product. Sign and unwrap through a key provider; do not invent customer-facing CA ceremony.
- Production integrations (object storage, billing, durable block storage, email) use narrow provider interfaces, as does the in-process sealed-secret key manager. Do not implement a new distributed database, workflow engine, storage engine, or global edge network here.
- Overlay dual-stack (1.1) is independent of the WireGuard underlay. Agent `advertise_addr` remains the IPv6 host identity, while the separately advertised WireGuard endpoint may use IPv4 or IPv6. Hosts still require IPv6 for the current host-identity model.

## Product gates

Gates are about a running product, not about emptying a folder.

### Dogfood

A known person can connect a typical GitHub repository, set secrets, deploy, and reach a process that bound `0.0.0.0` or `::`. Status, health, crashes, and timestamps are truthful. Auto-deploy can be turned off. Delete does not instantly destroy recoverable resources.

Requires [01-running-service.md](01-running-service.md). Only 1.2 (secrets in the console) is still open.

### Design-partner beta

Someone else can run a **real app** on the platform, and hosting their code does not endanger the platform. A real app is a web service plus a database plus a worker, on the partner's own domain over HTTPS, with secrets, migrations, and a volume that survives redeploys (not node loss; no backups, the same as Railway's baseline).

The gate is the ★ items in the queue:

- **Features:** 1.2 secrets, 2.8 domains and TLS, 4.2 reference variables, 6.1 basic volumes.
- **Safety:** 2.4a, 2.4b, 2.6, 2.7a, 2.9, 2.15. These cover source that does not live on one node's disk, a build boundary that holds against hostile code, digest-pinned runtime identity, an ingress that is not a single Caddy, logs that outlive a ClickHouse blip, and a container that cannot reach the platform's own control surface.

No GA promise. The rest of 02 (durable-work consolidation, key lifecycle, the per-node sync rewrite, onboarding polish) is real work a design partner will never see. It has to happen; it does not have to happen first.

### Multi-tenant

More than one team can share the installation. It needs workspaces with members, API tokens and a CLI, workspace limits, audit, deploy notifications, and platform backup.

Requires the "More than one team" section of the queue.

### Paid

Usage metering, Stripe plans with a hard spend limit, security scanning in CI, and an external penetration test. The GA-proof program (topology harness, fault and load suites, SLOs, DR drills) is parked in [freeze.md](freeze.md) and unparks when paid GA is actually scheduled.

## Work queue

Landed work is recorded in [landed.md](landed.md), parked work in [freeze.md](freeze.md). Old `x.y` IDs are in parentheses.

★ marks the [design-partner beta](#design-partner-beta) minimum. `Size` is an order of magnitude (S/M/L/XL), not an estimate.

**Priority is features a user touches.** When choosing between items with met dependencies, prefer the one that lets someone run an app they could not run before. Anything that is a platform-category checkbox (operator dashboards, programs, proofs, second sources of truth for existing settings) belongs in [freeze.md](freeze.md) until a real user needs it.

### Now: a real app runs on it

| # | Item | Size | File |
| --- | --- | --- | --- |
| ★ 1.2 | Sealed secrets in the console (2.4) | S | [01](01-running-service.md#12-sealed-secrets-in-the-console) |
| ★ 6.1 | Basic volumes (7.1, 7.2) | M | [06](06-stateful.md#61-basic-volumes) |
| ★ 2.8 | Custom domains and TLS (5.3) | M | [02](02-host-untrusted-code.md#28-domain-and-certificate-lifecycle) |
| ★ 4.2 | Reference variables (8.7) | M | [04](04-developer-surface.md#42-reference-variables) |
| 4.4 | Pre-deploy jobs (8.6) | M | [04](04-developer-surface.md#44-pre-deploy-jobs) |
| 3.7 | Public HTTP and TCP exposure (5.4) | M | [03](03-operate-multi-tenant.md#37-explicit-http-exposure) |
| 3.5 | Workload metrics (3.1) | M | [03](03-operate-multi-tenant.md#35-workload-and-platform-metrics) |
| 3.6 | Metrics graphs and log search (3.2) | M | [03](03-operate-multi-tenant.md#36-service-and-environment-observability-views) |
| 2.14 | Core onboarding path (8.9) | M | [02](02-host-untrusted-code.md#214-core-onboarding-path) |

Several landed or in-review backends have no console yet, and some of that UI is a feature users need, not polish: the log viewer ([2.9 handoff](../frontend-handoff/2.9.md)) and build cancellation/progress ([2.5 handoff](../frontend-handoff/2.5.md)). Treat those handoffs as part of this tier. The same goes for [1.8](../frontend-handoff/1.8.md) delete/restore and [2.3a](../frontend-handoff/2.3a.md) secrets. Operator-only handoffs (2.1, 2.4b, and the operator half of 2.5) wait for 3.10.

### Alongside: host untrusted code safely

| # | Item | Size | File |
| --- | --- | --- | --- |
| 2.1 | Durable-work package (2.7) | M | [02](02-host-untrusted-code.md#21-durable-work-package) |
| 2.3b | Platform signing key lifecycle (2.3) | M | [02](02-host-untrusted-code.md#23b-platform-signing-key-lifecycle) |
| ★ 2.4a | Build execution boundary (4.2) | M | [02](02-host-untrusted-code.md#24a-build-execution-boundary) |
| ★ 2.4b | Hardened build isolation backend (4.2) | XL | [02](02-host-untrusted-code.md#24b-hardened-build-isolation-backend) |
| 2.5 | Build leases and cancellation (4.3) | M | [02](02-host-untrusted-code.md#25-build-leases-and-cancellation) |
| ★ 2.6 | Deploy-by-digest (4.5) | S | [02](02-host-untrusted-code.md#26-deploy-by-digest) |
| ★ 2.7a | xDS control plane and Caddy cutover (5.2) | L | [02](02-host-untrusted-code.md#27a-xds-control-plane-and-caddy-cutover) |
| 2.7b | Envoy fleet (5.2) | M | [02](02-host-untrusted-code.md#27b-envoy-fleet-and-availability-policy) |
| ★ 2.9 | Durable bounded logs (3.3) | M | [02](02-host-untrusted-code.md#29-durable-bounded-logs) |
| 2.10 | Incremental per-node allocation sync | M | [02](02-host-untrusted-code.md#210-incremental-per-node-allocation-sync) |
| 2.11 | Scoped identity policy | M | [02](02-host-untrusted-code.md#211-scoped-identity-policy) |
| 2.12 | Environment-scoped WireGuard peering | M | [02](02-host-untrusted-code.md#212-environment-scoped-wireguard-peering) |
| ★ 2.15 | Workload egress guardrails (5.5) | M | [02](02-host-untrusted-code.md#215-workload-egress-guardrails) |

2.2 landed; see [landed.md](landed.md#22-source-object-storage). Most of this table is already in review. What is open is 2.6, 2.7b, 2.15, and the partial 2.11/2.12.

2.10–2.12 delete more code than they add and get harder the longer other code assumes the full snapshot and full mesh. They are not ★, but do not let them drift to the end.

### Then: more than one team on one installation

| # | Item | Size | File |
| --- | --- | --- | --- |
| 3.1 | Workspaces and members (6.1) | M | [03](03-operate-multi-tenant.md#31-organizations-and-rbac) |
| 3.2 | API tokens (6.3) | S | [03](03-operate-multi-tenant.md#32-scoped-api-credentials-and-sessions) |
| 3.13 | Platform CLI (8.2) | M | [03](03-operate-multi-tenant.md#313-platform-cli) |
| 3.3 | Workspace limits (6.4) | S | [03](03-operate-multi-tenant.md#33-resource-quotas) |
| 3.4 | Audit history (6.2) | S | [03](03-operate-multi-tenant.md#34-audit-history) |
| 3.9 | Deploy notifications and webhooks (3.4) | M | [03](03-operate-multi-tenant.md#39-monitors-notifications-and-webhooks) |
| 3.14 | Per-project build cache (4.4) | M | [03](03-operate-multi-tenant.md#314-tenant-safe-build-cache) |
| 3.11 | Platform backup and restore (2.5) | S | [03](03-operate-multi-tenant.md#311-platform-backup-and-restore) |

### Then: charge for it

| # | Item | Size | File |
| --- | --- | --- | --- |
| 5.1 | Usage meter (6.5) | M | [05](05-commercial.md#51-victoriametrics-billing-meter) |
| 5.2 | Plans and spend limit (6.6) | L | [05](05-commercial.md#52-plans-credits-and-spend-controls) |
| 7.1 | Security checks in CI (9.5) | M | [07](07-ga-proof.md#71-security-checks-in-ci) |

### Parked

2.13, 3.10, 3.12, 3.15–3.19, 4.1, 4.3, 4.5, 5.3, 5.4, 6.3, 6.4, 6.5, 7.2, 7.3, and 7.5. Each entry in [freeze.md](freeze.md) records why it was parked and what unparks it.
