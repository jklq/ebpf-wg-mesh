# 2 — Host untrusted code without a shared disk

These items make the dogfood loop safe to offer a design partner. Control-plane replicas and failover-aware durable agents already exist; replicas still share node-local keys. Production source snapshots live in operator-provided S3-compatible object storage (2.2 is done; see [landed.md](landed.md#22-source-object-storage)). Builds still run as a host process. Ingress is still one Caddy. Agents still get a full-cluster identity catalog over a full WireGuard mesh, and allocation delivery still uses complete per-node snapshots instead of bounded diffs.

Do not wait to empty this file before starting 3.x items that have no dependency here. Do wait to invite a second tenant’s source onto a shared builder until 2.4b exists.

Not every item here is partner-visible. The **design-partner minimum** is 2.2, 2.4a, 2.4b, 2.6, 2.7a, 2.9, and 2.15 — source that does not live on one disk, someone else’s code that does not run in your host process, digest-pinned images, an ingress that is not one Caddy, logs that survive a backend blip, and a container that cannot reach the platform’s own control surface. Everything else in this file is real work that no design partner will ever see; schedule it accordingly.

Extend the VM harness as each item needs a new topology component; 2.13 is done when the listed topology exists.

## 2.1 Durable-work package

Was: 2.7
Status: in review
Depends on: none

Not partner-visible. This is consolidation of code that already exists in triplicate — `github_work_items`, `source_work_items`, and `github_webhook_deliveries` are the same table written three times, and `claimNextSourceWorkItem` hand-rolls stale reclaim. It unblocks no product gate on its own. Prefer writing it *as* the deletion GC loop 1.8 needs and generalizing on the second caller, over landing it standalone ahead of work a user can see.

Implemented: `internal/controlplane/durablework` is the shared CockroachDB-backed queue (stable kind/ID, dedup key, scoped resource identity, pending/leased/succeeded/failed/dead states, attempt count/limit, owner epoch fencing, lease expiry with heartbeat, sanitized last error, backoff available-at). Claim, heartbeat, complete, and fail are single compare-and-swaps on (owner, epoch). Source work is the first migrated caller and `source_work_items` is deleted; delivery enqueues spec-changed work in the same product transaction via `EnqueueTx`. Bounded jittered retry, dead-letter inspection (`ListDead`/`Get`), a queue-lag query, and the external-effect intent rule with the source-sync worked handler are documented in the package. Tests cover multi-replica claim contention, worker death before/after the effect boundary, stale-owner late commit, retry exhaustion, and resurrection.

Remaining: migrate `github_webhook_deliveries` and `github_work_items` when next touched. No scheduled pruning of terminal rows yet (`PruneTerminal` exists for operators). Console dead-letter view and its RPC are deferred; see `docs/frontend-handoff/2.1.md`.

Prompt:

```text
Consolidate the duplicated control-plane work queues into one small CockroachDB-backed durable-work package. It replaces github_work_items, source_work_items, and github_webhook_deliveries, and it is not a workflow DSL. A work record needs a stable kind and ID, a unique deduplication key, scoped resource identity, pending/leased/succeeded/failed/dead state, attempt count and limit, owner ID, an owner epoch that increments on every claim, lease expiry with heartbeat, sanitized last error, an available-at time, and timestamps. Enqueue in the same transaction as the product-state mutation that requires it. Claim, heartbeat, complete, and fail must each be a single compare-and-swap on owner and epoch, so a stalled owner cannot commit after takeover without a separate fencing protocol layered on top. Provide bounded retry with jitter, a terminal dead state inspectable by query, and one queue-lag gauge. Document, with one worked handler, the rule that a handler performing an external effect persists intent before the call and records the observed outcome after it — do not build a generic two-phase intent framework to enforce it. First merge is the package plus one caller, deleting that caller's bespoke table. Migrate the others only when you next touch them. Test multi-replica claim contention, worker death before and after an external-effect boundary, and a stalled owner attempting a late commit after takeover.
```

## 2.3b Platform signing key lifecycle

Was: 2.3 (split)
Status: open
Depends on: [2.3a](landed.md#23a-secret-envelope-key-provider) for the provider contract.

Not partner-visible. Every credential these keys sign is short-lived, so rotation is an overlap window, not a ceremony.

Prompt:

```text
Stop generating the internal CA, registry token signer, user-assertion verifier material, and session signing keys under CONTROLPLANE_STATE_DIR, where replicas cannot agree on them. Load them through the same KeyProvider contract as 2.3a so key state is shared and consistent across replicas. Because every credential these keys sign is short-lived, rotation is: introduce a new active key, verify against both the active and retiring keys for longer than the longest credential lifetime, then retire. Sign only with the active key. Provide operator commands to start and finish a rotation, and a documented lifetime table showing how long the overlap must be for each credential type. Do not build an HSM or external-signer contract, a compromise-response runbook program, or a certificate-authority product. Test rotation across live sessions, agent certificate renewal, registry pulls, and replica restart, and prove a replica that starts mid-rotation accepts credentials signed by either key.
```

## 2.4a Build execution boundary

Was: 4.2 (split)
Status: open
Depends on: 2.2 so the executor reads a verified snapshot rather than a builder-local checkout.

Design-partner minimum. This item creates the seam and the policy; 2.4b makes the boundary real.

Prompt:

```text
Define a BuildExecutor interface that every build goes through, and make the current in-process path one implementation behind it. Each execution receives a read-only verified source snapshot, an isolated writable workspace and BuildKit endpoint, push credentials scoped to exactly one repository, explicit CPU/memory/disk/PID/time limits, and a restricted network policy — all expressed as executor inputs rather than ambient host state. The executor destroys its workspace and verifies cleanup on completion, cancellation, and worker death. Persistent cache data must be content-addressed so it cannot carry state between projects. This item does not yet claim a hardened boundary; it establishes the seam, the limits, and the credential scoping that 2.4b enforces, and the development executor must be labeled as non-isolating in production startup output. Test limit enforcement, credential scope, timeout, cancellation, and cleanup after worker death.
```

## 2.4b Hardened build isolation backend

Was: 4.2 (split)
Status: open
Depends on: 2.4a

Design-partner minimum, and the largest item in this file. Do not read the queue as uniform units of work.

Prompt:

```text
Add a production BuildExecutor backed by an operator-selected microVM or hardened sandbox technology, and make production refuse the development executor. A customer build must not reach host sockets, builder credentials, sibling build caches, control-plane credentials, or another project's files, and it must hold up against a hostile build rather than merely an uncooperative one. Do not build a VM orchestration platform in this repository — integrate a narrow backend and keep lifecycle and cleanup in the executor interface from 2.4a. Add adversarial tests for filesystem escape, host socket access, fork bomb, disk exhaustion, network denial, and cross-project cache access, and run the same suite against both executors so the development one is honestly labeled rather than silently weaker.
```

## 2.5 Build leases and cancellation

Was: 4.3
Status: open
Depends on: 2.1 if build claims move onto the shared durable-work package; otherwise keep the existing build-lease table and converge later.

Weighted fairness and quota-driven admission moved to 3.19. There is no second tenant to be fair between yet.

Prompt:

```text
Evolve the CockroachDB build queue into a durable lease-based scheduler without introducing Temporal or another general workflow system. Builders claim work transactionally with an owner epoch and lease expiry, heartbeat that lease, and may complete only by compare-and-swap on the current epoch. Fencing matters more here than for control-plane loops because a build's external effect is a registry push that outlives the lease. User cancellation and supersession must prevent a late completion from publishing an image or starting a rollout. Retry transient worker loss with a bounded attempt count while treating deterministic source or build failures as terminal. Enforce a per-workspace and a global concurrent-build cap, a build timeout, and a maximum queue age. Expose attempt history, cancellation progress, and an operator drain control. Test worker death, split ownership, late completion after cancellation, and cap release on every terminal path.
```

## 2.6 Deploy-by-digest

Was: 4.5
Status: open
Depends on: 1.6 so Railpack and Dockerfile builds both emit the artifact record.

Design-partner minimum. Mutable tags must never be the runtime identity of what is scheduled.

Built-image deployments already validate and persist a digest-pinned runtime reference, and deployment actions reuse that reference. The remaining contract is the immutable artifact record, direct-image tag resolution, skipped rebuilds of an already-built source, and retention.

Prompt:

```text
Preserve the existing digest-pinned build completion and deployment-action behavior, and create an immutable artifact record for every successful build containing source snapshot digest, commit SHA, build recipe and builder version, image manifest digest, build actor, and timestamps. Make that artifact, rather than an unstructured image string on the deployment, the source of runtime identity. Direct-image deploys must resolve a tag to a digest at deploy time and store the same artifact shape; mutable tags remain user input only. When the same source has already produced an image, skip the build and deploy that image (with the new environment’s variables). Preserve exact artifacts needed for rollback according to retention policy. Test tag mutation after resolve (the stored digest still runs). Do not add SBOM generation, image signing, vulnerability-policy gates, or multi-arch selection in this item.
```

## 2.7a xDS control plane and Caddy cutover

Was: 5.2 (split)
Status: open
Depends on: none. One Caddy is currently a product outage and the wrong apply protocol.

Design-partner minimum.

Prompt:

```text
Replace Caddy with Envoy driven by the control plane as xDS authority. The control plane serves a versioned snapshot (CDS/EDS/LDS/RDS, SDS once certificates exist) derived from CockroachDB; Envoy ACK/NACKs the applied revision, retains last-known-good on NACK, and converges after restart or partition. Snapshot computation must be canonical and deterministic so replicas racing to compute it produce identical bytes, and a rollout must never publish partially. Route only ready, non-draining allocations, support multiple replicas per service, and remove a backend from EDS before destructive shutdown. Remove Caddy from the production path, localteststack, and VM harness. A single Envoy instance is acceptable in this item; the fleet is 2.7b. Do not implement Envoy, a WAF, or a CDN in this repository. Failure tests: NACK of a bad snapshot, delayed apply, Envoy restart, and split control-plane ownership of snapshot publication.
```

## 2.7b Envoy fleet and availability policy

Was: 5.2 (split)
Status: open
Depends on: 2.7a

Prompt:

```text
Run a fleet of interchangeable Envoy instances against the xDS authority from 2.7a. Track per-instance applied revision and health, and define a configured availability policy that makes ingress readiness depend on enough instances sitting at the current revision rather than on any single instance. Provide per-instance status and a config diff without secrets. One instance being down, stale, or NACKing must not withdraw healthy traffic or block a rollout that the policy still satisfies. Extend the VM harness to at least two instances. Failure tests: one Envoy down, one Envoy stuck on an old revision, an instance rejoining behind the current revision, and a rollout that cannot satisfy the availability policy.
```

## 2.8 Domain and certificate lifecycle

Was: 5.3
Status: open
Depends on: 1.8 so deleted hostnames cannot be rebound during grace. 2.7a so certs are pushed to Envoy via SDS rather than Caddy automatic HTTPS.

Prompt:

```text
Turn domain bindings into an explicit verification and certificate lifecycle. Persist requested, verification-pending, verified, certificate-pending, active, degraded, and removing states with safe reason codes and timestamps. Prevent hostname takeover by proving DNS ownership according to the domain type, recheck ownership periodically, and avoid serving a new customer’s workload on a hostname retained from a deleted project. Issue certificates through a narrow ACME provider contract (not Caddy); push materials to Envoy over SDS with rate-limit awareness, renewal monitoring, challenge cleanup, and last-known-good certificate behavior. Show exact DNS records, observed values, certificate expiry, and actionable errors in the console. Generated domains must be collision-resistant and reserved transactionally. Test conflicting claims, dangling CNAMEs, rebinding after deletion grace, issuance failure, renewal failure, and certificate expiry alerts.
```

## 2.9 Durable bounded logs

Was: 3.3
Status: open
Depends on: none

Design-partner minimum. This item is the pipeline; console search surfaces are 3.6.

Prompt:

```text
Harden the existing ClickHouse log path for multi-tenant production use. Preserve runtime, build, deploy, HTTP, and network log types; add structured attributes for known platform events without parsing arbitrary customer output; define ordering and duplicate handling across reconnects; and retain the raw line exactly within a documented size limit. Agents and builders need bounded disk-backed spooling, batching, backpressure, retry, and explicit dropped-line counters so a ClickHouse outage cannot consume unbounded memory or block workload reconciliation. Enforce per-allocation rate and burst limits, tenant retention policies, and safe deletion after project expiry. Reads need authorized time-range queries with pagination or streaming, and must report an explicit gap where data was dropped rather than silently closing it. Test backend outage, retry duplication, oversized lines, abusive log rates, retention, and tenant isolation.
```

## 2.10 Incremental per-node allocation sync

Status: open
Depends on: none. Replaces the complete per-node snapshot as the steady-state wire format while preserving the durable acceptance and recovery invariants already implemented.

Agents already persist their accepted node snapshot, allocation generations, runtime identities, pending operations, and observation cursor. They reconcile before connecting, report inventory in hello, accept only complete agent-scoped snapshots under fenced authority, and avoid restarting matching healthy allocations. The remaining problem is that every change and reconnect still sends the complete per-node configuration. This item and 2.11–2.12 remove that coupling, and they get harder the longer other code assumes the snapshot.

Prompt:

```text
Replace complete per-node snapshot delivery with a checkpoint-plus-diff protocol. The control plane remains authoritative for placement. After reconciling the allocation inventory and accepted cursor sent in hello, it sends only bounded start, update, and stop changes for allocations assigned to that agent, ordered by a monotonically increasing per-node revision. A checkpoint may establish or repair the desired set on initialization, recovery, compaction, or cursor mismatch, but an unchanged reconnect must not resend the full node configuration. Preserve the current durable staging/publication boundary, agent-scoped removal authority, expiry and epoch fencing, session takeover, recovery quarantine, idempotent acknowledgement, and no-restart behavior for matching healthy allocations. Identity/policy (2.11), WireGuard peers (2.12), credentials, and replica discovery are independently versioned messages, not allocation changes. Cap each payload and retained diff history; fall back to a checkpoint when the agent's cursor is no longer available. Test reconnect without container restart or full resend, missed stop, compacted-history recovery, control-plane failover mid-sync, stale-epoch diffs, and a node that returns with extra or unowned containers.
```

## 2.11 Scoped identity policy

Status: partial
Depends on: 2.10 so policy is not piggy-backed on a cluster snapshot. 1.1 if both overlay families are present.

The fail-closed pool deny stays. The cluster-wide identity catalog on every node does not.

Implemented: agent identity catalogs now include only hosted environments, including remote same-environment allocations. Assignment fanout uses environment membership before and after the change, so removing the last allocation clears the former host's identities without updating unrelated environments. Focused unit and integration tests cover scoped catalogs, unrelated-environment growth, and removal.

Remaining: independent identity/policy delivery under 2.10 and the Envoy backend-scoping portion. Packet-level same-environment allow and cross-environment/unknown deny verification for this change still needs a privileged Linux host.

Prompt:

```text
Give each agent a pool deny plus exact allow entries only for network identities in environments that agent currently hosts, including remote allocations in those environments so same-environment east-west still works. Do not send identities for environments the node does not host. Envoy instances receive the backends they route, not the mesh catalog. Adding or removing an allocation updates only agents in that environment and the Envoy instances that publish it. Identity removal must still fail closed: unknown destinations and cross-environment traffic are denied on both overlay families. Test that a node hosting only environment A has no environment-B allows, that growth in B does not change A’s agents, that same-environment cross-node traffic still works, and that removing the last A allocation on a node drops A’s allows.
```

## 2.12 Environment-scoped WireGuard peering

Status: partial
Depends on: 2.11 so peers follow who is allowed to talk. 2.7b so ingress instances are the north-south peers.

Full mesh does not scale and is not required for fail-closed policy.

Implemented: shared-environment indexes derived from non-lost assignments scope agent-agent peers. Self, retired, and revoked peers are excluded; AllowedIPs remain each peer's dual overlay prefixes. Last-shared-allocation removal drops the peer, and peer changes fan out only to shared-environment members. Focused unit and integration tests cover disjoint environments, shared-environment addition/removal, duplicate memberships, lost allocations, and scoped endpoint updates.

Remaining: agent-to-Envoy peering under 2.7b and independent peer delivery under 2.10. Packet-level cross-node connectivity, denial without a tunnel, and Envoy backend reachability still need Linux topology verification.

Prompt:

```text
Stop forming a WireGuard peer between every pair of agents. Create and maintain tunnels only (1) between agents that currently share at least one environment and (2) between those agents and the Envoy instances that publish their allocations. AllowedIPs on each peer are only that peer’s overlay prefixes, not the whole cluster. When the last shared environment leaves a pair of nodes, tear the peer down. Control-plane mTLS stays off the mesh. The IPv6 host identity remains separate from the address-family-neutral WireGuard endpoint. Test: disjoint environments produce no agent-agent peer; adding a shared environment creates a peer; removing it destroys the peer; east-west same-environment still works; cross-environment stays denied without a tunnel; Envoy can still reach ready backends.
```

## 2.13 Production-like topology harness

Was: 9.1
Status: open
Depends on: none. Grow the existing testvm as 2.2, 2.4b, 2.7b, and 2.9 need components. Done when the topology below exists.

The harness now proves two control-plane processes, live-owner takeover, and two-agent reconnect/failover against one colocated CockroachDB process. The remaining work is to turn that proof harness into the distributed topology below.

Prompt:

```text
Extend the OpenTofu/VM harness into a production-like disposable topology with multiple control-plane replicas, a CockroachDB cluster, VictoriaMetrics, ClickHouse, object storage, registry, at least two Envoy instances, builders, and at least three agents across distinct failure domains. Keep external managed-provider behaviors behind test doubles or lightweight compatible services where provisioning the real provider is inappropriate. Generate per-run credentials, expose no admin service publicly, collect sanitized artifacts, and guarantee teardown on success, failure, or interruption. The harness must deploy the actual built binaries and configuration profiles rather than alternate test implementations. Produce a machine-readable topology manifest and health summary so scenario tests can target components deterministically.
```

## 2.14 Core onboarding path

Was: 8.9
Status: open
Depends on: 1.5, 1.6, 1.7, 1.8 so empty states, builder choice, auto-deploy, and deletion grace are real.

This is the human path through the loop, not an accessibility program.

Prompt:

```text
Refine the console around the supported path: create or choose a project, connect an authorized GitHub repository or direct image, inspect detected build configuration, create a service, review staged changes, deploy (or see auto-deploy), watch state including crash evidence, and reach a healthy endpoint. Add useful empty states and error recovery that preserves user input. Destructive actions must state scope and recovery. Do not add decorative templates. Add a Playwright journey covering failed build recovery, deploy, rollback, and deletion grace.
```

## 2.15 Workload egress guardrails

Was: part of 5.5, split out of 3.8
Status: open
Depends on: 1.1 so enforcement covers both families. Relates to 2.11, which governs same-environment identity.

Design-partner minimum. This is what stops a hostile container from reaching the platform itself.

Prompt:

```text
Make the platform's own attack surface unreachable from a customer container. Regardless of workload configuration, deny workload traffic to cloud metadata addresses, host and management networks, control-plane and agent administrative endpoints, registry credential endpoints, and every other tenant's overlay prefixes. Enforce close to the workload on both overlay families, fail closed on agent restart, and cover attempts that route through mapped, translated, or tunneled addresses. Same-environment private traffic continues to be governed by workload identity (2.11) and is not treated as external egress. Denials must be diagnosable by an operator without leaking destination data across tenants. Test metadata-address access, host-network access, cross-tenant overlay access, agent restart, and bypass through an alternate address family.
```
