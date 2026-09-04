# 3 — More than one team on one installation

These items make the beta loop shareable. They are not a reason to delay dual-stack, secrets, health, Railpack, or build isolation.

3.12 and 3.13 may start as soon as 3.1 and 3.2 exist, even if observability is unfinished. Cover only capabilities that already exist; extend the API as later items land.

## 3.1 Organizations and RBAC

Was: 6.1
Status: open
Depends on: none

Prompt:

```text
Introduce workspaces as the billing and organizational owner of projects while preserving personal ownership as a one-member workspace. Model invitations, membership lifecycle, and explicit workspace and project roles with capabilities rather than scattered role-name checks. At minimum distinguish administration, billing, project management, deployment, configuration/secrets management, log/metrics viewing, and read-only access; production environments must be restrictable separately. Centralize authorization in the control plane and apply it consistently to gRPC methods, streaming/event endpoints, console loaders/actions, future public API calls, webhooks, and support tooling. Membership changes must take effect promptly for live sessions, never expose secret values to viewers, and prevent removing the last owner. Add invitation expiry/revocation, project transfer, ownership transfer, and tests that enumerate every API capability across roles and environment restrictions.
```

## 3.2 Scoped API credentials and sessions

Was: 6.3
Status: open
Depends on: 3.1

Prompt:

```text
Add user, workspace, project, and environment-scoped API credentials with named capabilities, creation metadata, optional expiry, last-used time, and explicit revocation. Store only a strong token hash plus a short lookup prefix, display the plaintext once, use constant-time verification, rate-limit failures, and make revocation effective across all control-plane replicas. Tokens must never gain more capability than their creator and must be rejected for browser-only actions. Add session revocation, secure cookie defaults, CSRF protection for browser mutations, and a configurable absolute session expiry. Every use should carry an actor identity into audit events. Step-up authentication for sensitive production actions is 5.5, not this item. Provide console management and tests for scope boundaries, expiry, rotation, revocation races, and token leakage through logs/errors.
```

## 3.3 Resource quotas

Was: 6.4
Status: open
Depends on: 3.1. Volume-byte quotas stay zero/deny until the stateful provider exists.

Quota only over what the control plane already counts. Dimensions that require a metering system (build minutes, log ingestion, metrics cardinality, network egress, source storage) arrive with 5.1.

Prompt:

```text
Create a hierarchical quota model with operator defaults and workspace/project overrides. Cover only dimensions the control plane can already count authoritatively at admission time: projects, environments, services, replicas, requested CPU and memory, volume bytes, domains, concurrent builds, and API rate. Design the model so a metered dimension can be added later without reshaping it, but do not add build minutes, log ingestion, metrics cardinality, network egress, or source storage until 5.1 makes them measurable. Enforce quotas transactionally at resource creation or scaling, reserve capacity during in-progress changes, and release it reliably on cancellation or tombstoned deletion according to documented semantics. Distinguish product quota from temporary physical-capacity shortage and return machine-readable errors with current use, limit, and remediation. Do not silently overcommit resources whose isolation depends on hard limits. Provide usage views and operator override/audit flows. Test concurrent admission, failed rollout release, project transfer, quota reduction below current use, and enforcement consistency across console and API.
```

## 3.4 Audit history

Was: 6.2
Status: open
Depends on: 3.1. Deployment actions already exist and must emit events once this lands.

Prompt:

```text
Record an audit event for every authenticated mutation and every security-relevant system action: membership, roles, secrets, service config, deploy actions, domains, volumes, API tokens, billing limits, key rotation, support access, backups, restores, policy overrides, and deletion. Store actor type and stable ID, effective role, workspace/project/environment/resource scope, request correlation ID, action, safe before/after summaries, source IP metadata where trusted, result, reason, and UTC timestamp. Never store plaintext secrets, tokens, source archives, or unrestricted request bodies. Make event creation atomic with the mutation through a transaction or durable outbox, append-only to ordinary callers, queryable with authorization and pagination, exportable, and subject to a documented retention policy. Test redaction, failed attempts, system actors, retries, pagination, and tenant isolation. Do not add a hash-chain or external archival product.
```

## 3.5 Workload and platform metrics

Was: 3.1
Status: open
Depends on: none. Billable rollups wait for 5.1; this item is the operational series.

Prompt:

```text
Add a metrics path from agents, builders, ingress, and control-plane components into VictoriaMetrics. Collect allocation CPU time, throttling, working-set and RSS memory, OOM events, network ingress/egress bytes, ephemeral disk usage, persistent volume usage when available, restart counts, probe state, and allocation uptime from authoritative kernel/runtime counters. Collect build CPU, memory, duration, queue time, and transferred bytes separately. Tag samples with stable workspace/project/environment/service/allocation/build/agent identifiers and region or failure-domain labels, but never user-controlled secret values or unbounded log content. Define counter reset, allocation replacement, clock skew, scrape/remote-write retry, duplicate sample, and temporary-disconnection semantics. Metrics delivery must be buffered and bounded so an unavailable VictoriaMetrics cluster cannot exhaust an agent. Provide dashboards and tests proving aggregation across replicas without double counting.
```

## 3.6 Service and environment observability views

Was: 3.2
Status: open
Depends on: 3.5 and 2.9

Prompt:

```text
Build console observability views backed by VictoriaMetrics for resource series and ClickHouse for logs/events, including the environment-wide and deployment-scoped log search surfaces over the 2.9 pipeline, which must show an explicit gap where 2.9 reports dropped lines rather than presenting a continuous stream. A service view should correlate deployments with CPU, memory, OOMs, restarts, network traffic, probe transitions, replica count, request volume, error rate, and latency where ingress can observe them. An environment view should aggregate services while allowing drill-down by allocation and deployment generation. Support fixed and custom time ranges, stable downsampling, timezone-aware labels, missing-data explanation, and links from a chart anomaly to the relevant deployment and filtered logs. Query authorization must be enforced server-side from membership, not only by UI filtering. Bound query range, cardinality, and response size to protect shared backends, and test that one project cannot infer another project’s series or labels.
```

## 3.7 Explicit HTTP exposure

Was: 5.4
Status: open
Depends on: 2.7a so exposure is published through the xDS path.

Prompt:

```text
Model public HTTP endpoints separately from container ports: hostname, target port, TLS policy, request-size and timeout limits, optional WebSocket support, and trusted proxy/header behavior. Reject exposure of undeclared or unhealthy ports and ensure private-only services remain unreachable publicly. Record enough Envoy metrics in VictoriaMetrics to show request count, response class, latency, and rejected traffic without storing sensitive paths by default. Add console flows that explain the effect of exposure, plus end-to-end tests for HTTP, WebSocket, TLS, replica balancing, drain behavior, and cross-project isolation. Do not add public TCP/SNI routing in this item.
```

## 3.8 Egress policy

Was: 5.5
Status: open
Depends on: 2.15, which already made the platform's own surface unreachable, and 3.5 for accounting series.

The safety half of the old 3.8 is 2.15 and belongs much earlier. What remains here is a customer-facing feature, and it can wait for someone to ask for it.

Prompt:

```text
Add per-environment outbound policy as a customer-facing feature on top of the non-negotiable guardrails from 2.15. The default product policy may allow internet egress, but operators and authorized project roles must be able to deny all external egress, allow selected CIDRs and ports, or require traffic through an operator-provided egress gateway. Customer rules may only narrow what 2.15 already permits; they can never widen access to metadata addresses, host networks, control-plane administration, or other tenant overlays. Private same-environment traffic continues to use workload identities and must not be accidentally governed as public egress. Add connection and bandwidth accounting to VictoriaMetrics, configurable limits, SMTP and common abuse-sensitive port policy, and visible denial diagnostics that do not leak destination data across tenants. Verify IPv4 and IPv6 enforcement, DNS behavior, live rule updates, fail-closed agent restart, and that no customer rule can relax a 2.15 guardrail.
```

## 3.9 Monitors, notifications, and webhooks

Was: 3.4
Status: open
Depends on: 2.1 for webhook delivery, 3.5 for metric monitors.

Prompt:

```text
Create persisted monitors for deployment failure, crash loop, no healthy replica, CPU saturation, memory/OOM pressure, build queue delay, build failure, agent loss, ingress sync failure, log loss, and metrics ingestion lag. Evaluate metric monitors from VictoriaMetrics and state/event monitors from authoritative control-plane data, with configurable duration, threshold, severity, deduplication key, cooldown, and resolved notifications. Deliver in-app and email notifications through provider interfaces, plus project webhooks signed with a rotating secret. Webhook delivery needs an outbox, attempts, exponential retry, terminal failure visibility, replay, SSRF-safe URL validation, and no secret-bearing payloads. Give users test controls and event filters. Add deterministic rule tests and integration tests for firing, deduplication, resolution, retry, replay, and unauthorized configuration. Do not stub volume, backup, or billing monitors here.
```

## 3.10 Operator control room

Was: 3.5
Status: open
Depends on: 2.7b, 2.13, 3.5. Backup freshness waits for 3.11.

Prompt:

```text
Create an operator-only view and API that summarize control-plane replica health, CockroachDB and VictoriaMetrics reachability, ClickHouse ingestion, object storage, registry auth, builder capacity, build queue age, agent heartbeat age, allocation capacity, Envoy convergence, certificate expiry, notification failures, and garbage-collection backlog. Every degraded item must identify the affected scope and link to a runbook without exposing customer secrets. Define severity and ownership for each signal so the page is actionable instead of a wall of green checks. Exercise degraded dependencies in the local/VM harness. Do not add a diagnostic-bundle product in this item.
```

## 3.11 Platform backup and restore

Was: 2.5
Status: open
Depends on: 2.1, 2.2, 2.3a, 2.3b. Volume restore is out of scope until 6.3.

Prompt:

```text
Define and implement a coherent backup and restore procedure for CockroachDB platform schemas, console schemas, object-store metadata and archives, PKI/KMS references, registry trust, and required operator configuration. Backups must be automated, encrypted, retained on a documented schedule, monitored for freshness, and restorable into an isolated recovery environment without contacting production agents or mutating production ingress. Provide a restore command or documented orchestration that validates schema versions, referential integrity, source object availability, key access, and registry trust before enabling reconciler side effects. Establish explicit RPO and RTO targets and make the local/VM harness exercise a small backup-mutate-restore scenario. Restoration success means a service’s project, configuration, source snapshot, image reference, domains, deployment history, membership, and audit history are coherent—not merely that database tables can be imported.
```

## 3.12 Public customer API

Was: 8.1
Status: open
Depends on: 3.1, 3.2. Expose only capabilities that already exist; do not invent volumes, billing, or monitors here.

Prompt:

```text
Create a versioned public HTTP API backed by the same control-plane application services and authorization rules as the console; do not create a second source of business logic. Cover projects, environments, services, variables and secrets, source bindings, deployments and actions, domains/endpoints, volumes/backups, logs, metrics query links, usage, memberships, API tokens, monitors, and webhooks as each capability becomes available. Define consistent resource IDs, pagination, filtering, optimistic concurrency, idempotency keys, long-running operation resources, machine-readable errors, request IDs, rate-limit headers, and deprecation policy. Authenticate with the scoped credentials from the API-token item, emit audit events, redact secrets, and publish an OpenAPI document generated or verified in CI. The console may migrate incrementally but equivalent calls must produce identical authorization and state transitions. Add contract and cross-tenant tests.
```

## 3.13 Platform CLI

Was: 8.2
Status: open
Depends on: 3.12

Prompt:

```text
Add a typed CLI for login/token configuration, context selection, project and environment inspection, service creation, source or image deployment, variables and secrets, logs, deployment actions, scaling, domains, volumes/backups, usage, and status. Design commands for both interactive humans and CI: stable exit codes, stdout for requested data, stderr for progress, JSON output, --yes for confirmed destructive operations, no color when non-interactive, cancellable waits, and explicit workspace/project/environment flags that override saved context. Never put tokens or secret values in command history, process arguments where avoidable, debug output, or shell completion. Upload source as a deterministic bounded archive only if a future non-GitHub source path is intentionally supported. Generate shell completions and focused command-contract tests against the public API.
```

## 3.14 Tenant-safe build cache

Was: 4.4
Status: open
Depends on: 2.4b

Prompt:

```text
Add a documented build-cache model for Railpack and Dockerfile builds. Cache keys must include the source inputs, builder kind and version, architecture, relevant build configuration, and hashes—not plaintext—of build-time variables that legitimately affect the result. Permit sharing across environments of the same project where safe, prohibit cross-project private cache reads, and make public base-layer reuse explicit. Bound cache storage per workspace, track hits/misses and bytes, provide eviction and manual clear operations, and ensure revoked source or deleted projects eventually lose private cache material. Build secrets must use BuildKit secret mounts and must not affect final image layers, metadata, provenance, or logs except through an irreversible cache-input hash where required. Test cache poisoning, secret rotation, branch builds, environment reuse, eviction, and access isolation.
```

## 3.15 Image and artifact lifecycle

Was: 4.6
Status: open
Depends on: 1.6, 2.6

Prompt:

```text
Track references from active deployments, rollback retention, source builds, and legal deletion grace periods to registry manifests, layers, SBOMs, provenance, and source archives. Implement a mark-and-sweep lifecycle that never removes an artifact still reachable from an active or retained deployment, tolerates registry/object-store partial failure, and records tombstoned and physically deleted states. Define per-plan retention for failed builds, successful historic builds, logs, source archives, and images. Show users when an old deployment is no longer redeployable and why before its artifacts expire. Provide operator dry-run and reconciliation commands plus metrics for reclaimable bytes, failed deletes, and orphan discovery. Test shared layers, concurrent rollback, project restoration during grace, external objects missing early, and repeated collection.
```

## 3.16 Production registry contract

Was: 4.7
Status: open
Depends on: 2.6, 3.15

Prompt:

```text
Define the production requirements and integration contract for the external OCI registry while preserving embedded control-plane token minting and exact-repository authorization. The configured registry must use durable replicated object storage or another operator-proven durable backend, TLS, authenticated internal administration, manifest deletion compatible with artifact lifecycle, retention-safe garbage collection, health/readiness reporting, and backup or reconstruction procedures for metadata and trust configuration. Verify that short-lived builder push and agent pull capabilities cannot list or access sibling repositories, that revoked or deleted project scopes stop minting new tokens, and that active agents can cold-pull retained digests after restart. Add reconciliation for manifests expected by CockroachDB but missing from the registry and vice versa, VictoriaMetrics operational metrics, capacity alerts, and VM tests for registry restart, backend outage, partial push, garbage collection, signing-key rotation, and cold-node recovery. Do not implement an OCI registry server in this repository.
```

## 3.17 Failure and recovery scenarios

Was: 9.2
Status: open
Depends on: 2.13. Cover the components that exist; add stateful two-writer cases when 6.4 lands.

Prompt:

```text
Create a focused fault suite against the production-like topology. Kill and restart control-plane replicas, Cockroach nodes, agents, Envoy instances, builders, VictoriaMetrics, ClickHouse, object storage, registry, and network links at meaningful transition points. Verify that active healthy workloads stay reachable, old deployment generations remain serving during failed rollouts, stateless replicas reschedule, durable work resumes without duplicate effects, logs/metrics report bounded gaps honestly, and queued actions do not disappear. Include clock skew and expired credentials where feasible. Record recovery time and invariant failures as artifacts. Keep scenarios few and high-value rather than creating a combinatorial chaos framework. Do not invent stateful two-writer cases until 6.4 exists.
```

## 3.18 Load, scale, and noisy-neighbor limits

Was: 9.3
Status: open
Depends on: 3.3, 2.11, 3.19

Prompt:

```text
Define initial supported scale targets for workspaces, projects, environments, services, allocations, agents, concurrent builds, per-node alloc-set size, domains, log lines per second, VictoriaMetrics samples/cardinality, API requests, and ingress connections. Build reproducible load tests that exercise control-plane reads and mutations, scheduler placement, agent reconnect storms, rollout fan-out, log/metrics ingestion, deployment history, and console-critical queries at those targets. Measure latency percentiles, error rate, Cockroach contention, queue age, memory, CPU, and recovery after load. Include one intentionally noisy tenant and prove quotas protect others. Turn discovered cliffs into enforced product limits or engineering fixes; publish the limits and fail admission before undefined behavior. Do not require billing rollups in this item.
```

## 3.19 Build admission and fairness

Was: part of 4.3, split out of 2.5
Status: open
Depends on: 2.5 for the lease scheduler, 3.3 for the quota model.

Deliberately after 3.1/3.3. Weighted fairness between tenants is meaningless until there are tenants; 2.5's flat concurrency caps carry the single-tenant and design-partner stages on their own.

Prompt:

```text
Add tenant-aware admission and fairness to the build scheduler from 2.5. Claim order must apply weighted fairness across workspaces so one workspace cannot monopolize builders while another starves, and admission must reject new builds when the workspace's quota from 3.3 is exhausted, with a machine-readable error naming the limit and current use. Expose approximate queue position to the user. Keep the flat caps from 2.5 as the floor so removing the fairness policy degrades to correct-but-unfair rather than unbounded. Test starvation resistance with one heavy and several light workspaces, quota-exhausted admission rejection, quota release on every terminal path, and that fairness never delays a build past the maximum queue age from 2.5.
```
