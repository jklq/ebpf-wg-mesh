# Parked

Prompts here are not cancelled and not scheduled. They stay out of the [work queue](README.md#work-queue) so it only lists work that is actually next, and they keep their original IDs so dependency lines elsewhere still resolve. Record why an item was parked and what would unpark it. Move an item back into its numbered file when that condition is met.

## 2.13 Production-like topology harness

Parked: test infrastructure, not a product feature. The README already says to grow the testvm per item as each needs a component; keep doing that. Unparks when a fault or load suite (3.17, 3.18) is unparked and needs a distributed topology to run against.

Was: 9.1
Status: parked
Depends on: none. Grow the existing testvm as 2.2, 2.4b, 2.7b, and 2.9 need components. Done when the topology below exists.

The harness now proves two control-plane processes, live-owner takeover, and two-agent reconnect/failover against one colocated CockroachDB process. The remaining work is to turn that proof harness into the distributed topology below.

Prompt:

```text
Extend the OpenTofu/VM harness into a production-like disposable topology with multiple control-plane replicas, a CockroachDB cluster, VictoriaMetrics, ClickHouse, object storage, registry, at least two Envoy instances, builders, and at least three agents across distinct failure domains. Keep external managed-provider behaviors behind test doubles or lightweight compatible services where provisioning the real provider is inappropriate. Generate per-run credentials, expose no admin service publicly, collect sanitized artifacts, and guarantee teardown on success, failure, or interruption. The harness must deploy the actual built binaries and configuration profiles rather than alternate test implementations. Produce a machine-readable topology manifest and health summary so scenario tests can target components deterministically.
```

## 3.10 Operator control room

Parked: operator dashboard. Operators can read the same signals from VictoriaMetrics/Grafana and the existing operator RPCs. Unparks when there are enough operators or installations that ad-hoc dashboards stop being enough, or 7.2 is unparked.

Was: 3.5
Status: parked
Depends on: 2.7b, 2.13, 3.5. Backup freshness waits for 3.11.

Prompt:

```text
Create an operator-only view and API that summarize control-plane replica health, CockroachDB and VictoriaMetrics reachability, ClickHouse ingestion, object storage, registry auth, builder capacity, build queue age, agent heartbeat age, allocation capacity, Envoy convergence, certificate expiry, notification failures, and garbage-collection backlog. Every degraded item must identify the affected scope and link to a runbook without exposing customer secrets. Define severity and ownership for each signal so the page is actionable instead of a wall of green checks. Exercise degraded dependencies in the local/VM harness. Do not add a diagnostic-bundle product in this item.
```

## 3.12 Public customer API

Parked: a versioned public HTTP API with OpenAPI, deprecation policy, and LRO resources is a product in itself. The CLI (3.13) talks to the existing Connect services with scoped tokens instead. Unparks when a customer needs a stable, documented integration contract beyond the CLI.

Was: 8.1
Status: parked
Depends on: 3.1, 3.2. Expose only capabilities that already exist; do not invent volumes, billing, or monitors here.

Prompt:

```text
Create a versioned public HTTP API backed by the same control-plane application services and authorization rules as the console; do not create a second source of business logic. Cover projects, environments, services, variables and secrets, source bindings, deployments and actions, domains/endpoints, volumes/backups, logs, metrics query links, usage, memberships, API tokens, monitors, and webhooks as each capability becomes available. Define consistent resource IDs, pagination, filtering, optimistic concurrency, idempotency keys, long-running operation resources, machine-readable errors, request IDs, rate-limit headers, and deprecation policy. Authenticate with the scoped credentials from the API-token item, emit audit events, redact secrets, and publish an OpenAPI document generated or verified in CI. The console may migrate incrementally but equivalent calls must produce identical authorization and state transitions. Add contract and cross-tenant tests.
```

## 3.15 Image and artifact lifecycle

Parked: garbage collection of registry/object-store artifacts. Storage growth is an operator cost, not a user-facing gap; operators can prune manually. Unparks when registry or source storage cost becomes material, or users start losing rollback targets unpredictably.

Was: 4.6
Status: parked
Depends on: 1.6, 2.6

Prompt:

```text
Track references from active deployments, rollback retention, source builds, and legal deletion grace periods to registry manifests, layers, SBOMs, provenance, and source archives. Implement a mark-and-sweep lifecycle that never removes an artifact still reachable from an active or retained deployment, tolerates registry/object-store partial failure, and records tombstoned and physically deleted states. Define per-plan retention for failed builds, successful historic builds, logs, source archives, and images. Show users when an old deployment is no longer redeployable and why before its artifacts expire. Provide operator dry-run and reconciliation commands plus metrics for reclaimable bytes, failed deletes, and orphan discovery. Test shared layers, concurrent rollback, project restoration during grace, external objects missing early, and repeated collection.
```

## 3.16 Production registry contract

Parked: operator contract and verification for the external registry. Pull/push scoping already exists. Unparks when 3.15 is unparked, or before paid GA.

Was: 4.7
Status: parked
Depends on: 2.6, 3.15

Prompt:

```text
Define the production requirements and integration contract for the external OCI registry while preserving embedded control-plane token minting and exact-repository authorization. The configured registry must use durable replicated object storage or another operator-proven durable backend, TLS, authenticated internal administration, manifest deletion compatible with artifact lifecycle, retention-safe garbage collection, health/readiness reporting, and backup or reconstruction procedures for metadata and trust configuration. Verify that short-lived builder push and agent pull capabilities cannot list or access sibling repositories, that revoked or deleted project scopes stop minting new tokens, and that active agents can cold-pull retained digests after restart. Add reconciliation for manifests expected by CockroachDB but missing from the registry and vice versa, VictoriaMetrics operational metrics, capacity alerts, and VM tests for registry restart, backend outage, partial push, garbage collection, signing-key rotation, and cold-node recovery. Do not implement an OCI registry server in this repository.
```

## 3.17 Failure and recovery scenarios

Parked: GA proof, not a feature. Failure tests that matter live on the items they cover. Unparks when preparing for paid GA.

Was: 9.2
Status: parked
Depends on: 2.13. Cover the components that exist. Volume-backed services keep rejecting overlapping replacement; do not invent two-writer cases.

Prompt:

```text
Create a focused fault suite against the production-like topology. Kill and restart control-plane replicas, Cockroach nodes, agents, Envoy instances, builders, VictoriaMetrics, ClickHouse, object storage, registry, and network links at meaningful transition points. Verify that active healthy workloads stay reachable, old deployment generations remain serving during failed rollouts, stateless replicas reschedule, durable work resumes without duplicate effects, logs/metrics report bounded gaps honestly, and queued actions do not disappear. Include clock skew and expired credentials where feasible. Record recovery time and invariant failures as artifacts. Keep scenarios few and high-value rather than creating a combinatorial chaos framework. Do not invent stateful two-writer cases.
```

## 3.18 Load, scale, and noisy-neighbor limits

Parked: GA proof, not a feature. There is no scale to measure yet. Unparks when preparing for paid GA, or a real installation hits a scale cliff.

Was: 9.3
Status: parked
Depends on: 3.3, 2.11, 3.19

Prompt:

```text
Define initial supported scale targets for workspaces, projects, environments, services, allocations, agents, concurrent builds, per-node alloc-set size, domains, log lines per second, VictoriaMetrics samples/cardinality, API requests, and ingress connections. Build reproducible load tests that exercise control-plane reads and mutations, scheduler placement, agent reconnect storms, rollout fan-out, log/metrics ingestion, deployment history, and console-critical queries at those targets. Measure latency percentiles, error rate, Cockroach contention, queue age, memory, CPU, and recovery after load. Include one intentionally noisy tenant and prove quotas protect others. Turn discovered cliffs into enforced product limits or engineering fixes; publish the limits and fail admission before undefined behavior. Do not require billing rollups in this item.
```

## 3.19 Build admission and fairness

Parked: weighted fairness between tenants. 2.5's flat global and per-project caps are enough until builders are actually contended. Unparks when build queue age from one tenant visibly starves another.

Was: part of 4.3, split out of 2.5
Status: parked
Depends on: 2.5 for the lease scheduler, 3.3 for the quota model.

Deliberately after 3.1/3.3. Weighted fairness between tenants is meaningless until there are tenants; 2.5's flat concurrency caps carry the single-tenant and design-partner stages on their own.

Prompt:

```text
Add tenant-aware admission and fairness to the build scheduler from 2.5. Claim order must apply weighted fairness across workspaces so one workspace cannot monopolize builders while another starves, and admission must reject new builds when the workspace's quota from 3.3 is exhausted, with a machine-readable error naming the limit and current use. Expose approximate queue position to the user. Keep the flat caps from 2.5 as the floor so removing the fairness policy degrades to correct-but-unfair rather than unbounded. Test starvation resistance with one heavy and several light workspaces, quota-exhausted admission rejection, quota release on every terminal path, and that fairness never delays a build past the maximum queue age from 2.5.
```

## 4.1 Repository configuration as code

Parked: convenience. Every setting it covers is already configurable in the console; this adds a second source of truth plus merge/provenance rules. Unparks when users ask for it repeatedly, or PR environments (4.5) are unparked and need per-branch config.

Was: 8.3
Status: parked
Depends on: 1.6. Healthcheck path and timeout already exist as a deploy-time rollout gate.

Prompt:

```text
Define a small versioned platform.toml or platform.json schema for build and runtime settings that appropriately belong with source: builder kind, Dockerfile/context or Railpack settings, build command, watch paths, start command, pre-deploy command, declared ports, health checks, restart policy, graceful drain, replicas within allowed bounds, resource requests/limits, mount path references, and public endpoint intent. Never allow plaintext secret values, memberships, billing, or operator policy in this file. Parse configuration from the immutable source snapshot, record its digest and field provenance on the deployment, validate it before building, and make repository values override or merge with dashboard settings according to one documented rule. Show effective values and provenance in the console and API. Protect production from unreviewed dangerous changes through existing staged-change and RBAC rules. Publish a JSON schema and test versioning, invalid config, monorepo paths, and rollback.
```

## 4.3 Monorepo watch paths

Parked: convenience. Monorepos still deploy correctly; they just rebuild unaffected services. Unparks when monorepo users hit real build cost or deploy churn from unnecessary rebuilds.

Was: 8.5
Status: parked
Depends on: 1.6

Prompt:

```text
Let repository-backed services declare normalized include and exclude watch paths relative to their build context. On each verified source revision, compare changed paths from the GitHub API or immutable snapshots and queue a build only when the service is affected; a manual deploy must be able to override skipping. Persist a visible skipped deployment reason and the evaluated rule version. Coalesce repeated webhook work for the same service and commit, supersede queued older commits when safe, and reuse one source snapshot across affected services without sharing mutable build state. Path matching must reject traversal, have documented glob semantics, handle merge commits and unavailable diffs conservatively, and avoid skipping when correctness is uncertain. Add tests for root services, nested contexts, renames, deletes, large/unknown diffs, force-pushes, and concurrent deliveries.
```

## 4.5 Pull-request environments

Parked: large (webhook trust, forked-PR secrets, ephemeral quota) for a feature that is nice to have rather than needed to run an app. Unparks when the core feature set and quotas (3.3) are in, and users ask for preview environments.

Was: 8.4
Status: parked
Depends on: 1.8, 3.3, 2.4b. Do not copy production secrets onto forked PRs.

Prompt:

```text
Extend EnvironmentKind with ephemeral pull-request environments driven by verified GitHub App webhook state. A project may configure a persistent base environment, allowed repositories/target branches, resource/replica caps, secret-copy policy, and automatic expiry. On an eligible PR open or update, create or reconcile one isolated environment, copy only permitted configuration, deploy affected services from the exact head SHA, post or update one GitHub status/comment with URLs and state, and remove the environment after merge/close plus a grace period. Handle webhook replay, force-push, forked PR trust, bot PR policy, revoked installation access, and out-of-order events without deploying untrusted code with production secrets. Budget and quota ephemeral use separately, and test the complete lifecycle with fixture webhooks.
```

## 5.3 Fair-use and abuse safeguards

Parked: abuse controls matter once signup is open to strangers. Quotas (3.3) and per-allocation log limits (2.9) cover known tenants. Unparks when self-serve signup is opened.

Was: 6.7
Status: parked
Depends on: 3.3, 3.18, 3.19

Prompt:

```text
Protect shared infrastructure from abusive or accidentally pathological tenants. Add per-actor API token buckets, expensive-query budgets, build and deployment churn limits, log rate and metrics cardinality limits, webhook destination controls, registry request limits, SMTP/abuse-sensitive outbound port policy, and configurable account-risk holds. Limits must be hierarchical, observable, machine-readable, and degrade the offending scope without destabilizing other tenants. Security-sensitive limits must not be user-increasable; commercial limits may have audited operator overrides. Avoid brittle content classification inside the platform: integrate external fraud or abuse review only through a narrow status provider and retain human override. Provide a customer-visible reason and appeal/support reference without revealing detection internals. Load-test noisy-neighbor isolation and verify that one workspace cannot exhaust control-plane workers, ClickHouse, VictoriaMetrics, builders, registry, or ingress.
```

## 5.4 Account retention and closure

Parked: account closure and data-retention program. Project deletion with grace (1.8) already exists. Unparks when self-serve signup and billing exist, or a legal/compliance requirement lands.

Was: 6.8
Status: parked
Depends on: 1.6, 3.4, 5.1

Prompt:

```text
Define customer data categories and retention for identity, configuration, source snapshots, images, secrets, logs, metrics, usage ledger, invoices, audit events, backups, and support artifacts. Account closure must disable new work, settle or preserve required billing records, revoke credentials, tombstone resources, honor recovery grace, and then delete external artifacts through idempotent jobs while retaining only legally required records. Expose progress and failures to operators and the customer. Make retention configurable by plan where appropriate but never shorter than safety or billing invariants. Test cancellation during grace, partial provider deletion, and proof that closed tenants disappear from product queries.
```

## 7.2 SLOs and incident response

Parked: SLO and incident process. Needs real traffic to be meaningful. Unparks when preparing for paid GA.

Was: 9.6
Status: parked
Depends on: 3.5, 3.9, 3.10

Prompt:

```text
Define measurable service indicators and initial SLOs for control API availability, deployment queue/start success, healthy ingress availability, log and metrics ingestion delay, build completion, and stateless recovery. Source indicators from VictoriaMetrics and authoritative control-plane state, exclude only documented maintenance, and add multi-window burn-rate alerts with clear ownership. Create concise runbooks for every paging alert, an incident command and severity process, customer-impact assessment, status-page updates, security escalation, and blameless post-incident review. Integrate an external status-page and paging provider through narrow interfaces rather than building them here. Run game days for control-plane loss, region/agent loss, bad deploy, and credential compromise. Add volume and billing indicators only after those products exist.
```

## 7.3 Version skew and cutover

Parked: the repo does clean cutovers by policy; there is no fleet with version skew yet. Unparks when more than one installation is upgraded independently, or agents/builders are rolled separately from the control plane.

Was: 9.7
Status: parked
Depends on: none. This repo does not do rolling backward-compatible migrations yet.

Prompt:

```text
Agents and builders report version and capabilities; the control plane rejects unsupported combinations with actionable status rather than corrupting desired state. Database and protobuf cutovers are explicit convert-or-refuse steps with a backup, not dual-shape compatibility windows. Exercise eBPF program replacement without dropping fail-closed policy. Do not require one-version-back wire compatibility or SBOM-signed release trains in this item.
```

## 7.5 Continuous backup and disaster-recovery drills

Parked: automated DR drills. 3.11 (platform backup) and 6.3 (volume backup) must include one restore test each; scheduled drills come later. Unparks when preparing for paid GA.

Was: 9.4
Status: parked
Depends on: 3.11. Include durable volumes only after 6.3.

Prompt:

```text
Automate scheduled recovery exercises rather than treating backup creation as success. Restore CockroachDB, object metadata/source archives, key-provider references, registry trust, and representative durable volumes into an isolated environment. Verify project membership, secrets decryption, source integrity, deployment and audit history, image availability, domain intent without publishing production DNS, metrics billing watermarks, backup catalog, and application-level volume sentinels. Measure achieved RPO/RTO, retain a signed result, alert on missed or failed drills, and prevent the recovery environment from contacting production agents, ingress, billing, notifications, or webhooks. Include a documented regional-loss/manual bootstrap procedure and identify every external dependency the operator must restore first.
```

## 6.3 Volume snapshots, backups, and restores

Parked: volumes start node-local and without backups, the same as Railway's baseline. Users who need backups run their own dumps. Unparks when users are losing data they cared about, or the durable provider (6.4) is unparked.

Was: 7.3
Status: parked
Depends on: 6.1, 2.1. Emit audit events once 3.4 exists; do not wait for it.

Prompt:

```text
Implement manual and scheduled volume backups through the configured VolumeProvider or BackupProvider, with daily/weekly/monthly policies, retention, encryption, progress, failure state, and cost/size metadata. Define crash-consistent behavior honestly; optionally support pre/post hooks for application-coordinated backups without claiming consistency when hooks fail. A restore must create a new volume from the chosen backup, verify provider completion and size, stage an attachment change, and preserve the previous volume until the user explicitly deletes it after a grace period. Never restore destructively in place by default. Backup and restore actions must be authorized, audited, monitored for freshness, and safe under retry. Add restoration verification that mounts the recovered volume in isolation and checks a supplied sentinel, plus tests for partial snapshots, retention locks, schedule races, provider outage, and restore cancellation.
```

## 6.4 Durable volume provider

Parked: network or replicated block storage is past what users need to start. Basic volumes (6.1) are node-local: they survive redeploys and restarts, and are lost if the node is lost. Unparks when node loss has actually cost a user data, or users need to move a volume-backed service between nodes.

Was: 7.1, then 6.1
Status: parked
Depends on: 6.1 (basic volumes)

Prompt:

```text
Replace the production volume implementation based on agent-local directories with a VolumeProvider contract for operator-selected durable block or network storage. Keep local directories only as an explicitly non-durable development provider. A volume has provider identity, region/failure domain, requested and actual size, filesystem, mount options, lifecycle state, attachment generation, and exactly one writer unless a provider explicitly supports another mode. The control plane owns attachment intent and fences it with a monotonically increasing token; agents attach, format only once, mount at the configured absolute path, report observed identity, and refuse stale attachment generations. Scheduling must honor volume locality and provider availability. Never call RemoveAll on production volume data. Add provider conformance tests and simulate attach timeout, stale attachment, node loss, remount, provider outage, and accidental format attempts.
```

## 6.5 Storage monitoring and safety rails

Parked: basic volumes (6.1) already show usage and enforce size. Capacity monitors wait for 3.9, and storage billing waits for 5.1. Unparks when monitors (3.9) exist and users are hitting full volumes by surprise.

Was: 7.5
Status: parked
Depends on: 6.1, 6.3, 3.5, 3.9, 5.1 if storage is billed

Prompt:

```text
Collect volume provisioned bytes, used bytes, inode use where available, read/write bytes, IOPS, latency, attachment errors, backup age, and restore state into VictoriaMetrics using stable volume identity. Add warning and critical capacity monitors and surface them on service and volume views. Prevent shrink operations. Production volume deletion requires typed confirmation, a grace period with restore, and an audit event. Bill provisioned storage and backup storage from authoritative provider/lifecycle records reconciled with VictoriaMetrics, not from mutable console values.
```
