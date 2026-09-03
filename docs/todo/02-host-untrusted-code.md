# 2 — Host untrusted code without a shared disk

These items make the dogfood loop safe to offer a design partner. Control-plane replicas already exist; they still share node-local source and keys. Builds still run as a host process. Ingress is still one Caddy.

Do not wait to empty this file before starting 3.x items that have no dependency here. Do wait to invite a second tenant’s source onto a shared builder until 2.4 exists.

Extend the VM harness as each item needs a new topology component; 2.11 is done when the listed topology exists, not before object storage may start.

## 2.1 Durable-work package

Was: 2.7
Status: open
Depends on: none. Do this early so later GC, webhooks, ingress, backups, and billing do not invent a third claiming scheme.

Prompt:

```text
Create a small CockroachDB-backed durable-work package used by control-plane background operations instead of allowing each reconciler to invent subtly different claiming behavior. It is not a workflow DSL or general orchestration product. A work record needs a stable kind and ID, schema version, deduplication/idempotency key, scoped resource identity, pending/leased/succeeded/failed/dead state, available time, attempt count and limit, lease owner, monotonically increasing lease epoch used as a fencing token, lease expiry and heartbeat, sanitized last error, result metadata, and created/updated/completed times. Enqueue work atomically with the product-state mutation or transactional outbox that requires it. Claim atomically, and require the current lease epoch on heartbeat, completion, release, cancellation, and every state-changing callback so a stalled worker cannot commit after takeover. Handlers must make external effects idempotent with provider request keys where available, persist intent before performing an effect, record observation afterward, and reconcile ambiguous outcomes instead of assuming exactly-once delivery. Provide bounded exponential retry with jitter, terminal classification, dead-letter inspection/replay, queue lag and attempt metrics, operator drain controls, and cleanup/retention. Migrate GitHub deliveries and source work, build claims, notifications/webhooks, ingress publication, deletion garbage collection, backup/restore operations, billing rollups, scheduled jobs, and other long-running operations onto these primitives where their domain state machine does not require a more specific table. Add multi-replica tests that kill workers before and after each external-effect boundary, force lease expiry and stale completion, replay enqueue requests, and prove eventual convergence without duplicate product effects.
```

## 2.2 Source object storage

Was: 2.2
Status: open
Depends on: 1.6 for deletion grace. Production must stop requiring a shared filesystem of archives across control-plane replicas.

Prompt:

```text
Keep FileSourceArchiveStore for local development and add a production SourceArchiveStore backed by operator-provided S3-compatible object storage. Store content-addressed immutable archives by verified SHA-256 digest, stream uploads and downloads without loading the whole archive into memory, verify size and digest at both boundaries, and use conditional creation so duplicate snapshots converge safely. Persist object metadata and lifecycle state in CockroachDB, distinguish missing/corrupt/transient retrieval failures, and garbage-collect only objects that are unreferenced after the deletion grace period. Support configurable server-side encryption, endpoint, region, bucket, credential-file or workload-identity auth, timeouts, and bounded retries without logging credentials. Production mode must reject the filesystem provider. Add contract tests shared by file and S3-compatible implementations plus failure tests for partial upload, stale metadata, range reads, deletion races, and digest mismatch.
```

## 2.3 Externalize platform keys

Was: 2.3
Status: open
Depends on: 1.2 if envelope keys for secrets already exist, so rotation covers them.

Prompt:

```text
Replace production dependence on private keys generated under CONTROLPLANE_STATE_DIR with explicit key providers. Cover the internal CA, registry token signer, user-assertion verifier material, GitHub token encryption, session signing, and any envelope-encryption keys introduced for customer secrets. Support a local file provider for development and a production KMS/HSM or externally mounted signer contract that can sign or unwrap without exporting a long-lived root key into application logs or database rows. Model key IDs and active/retiring/retired states, allow overlapping verification during rotation, make new signatures use the active key, and provide operator commands and runbooks for scheduled rotation and compromise response. Control-plane replicas must see consistent key state. Refuse unsafe key deletion while dependent certificates, tokens, or ciphertext remain valid, and test rotation across live sessions, agent renewal, registry pulls, and replica restarts.
```

## 2.4 Isolate untrusted builds

Was: 4.2
Status: open
Depends on: 2.2 so the executor reads a verified snapshot rather than a builder-local checkout.

Prompt:

```text
Run each customer build inside a disposable isolation boundary stronger than a shared host process. Define a BuildExecutor interface and provide a local executor for development plus a production executor using an operator-selected microVM or hardened sandbox technology. Each execution receives a read-only verified source snapshot, an isolated writable workspace and BuildKit endpoint, scoped push credentials, explicit CPU/memory/disk/PID/time limits, and a restricted network policy. It must not access host sockets, builder credentials, sibling caches, control-plane credentials, or another project’s files. On completion or cancellation, destroy the execution environment and verify cleanup; persistent cache data must be content-addressed and tenant-safe. Do not build a VM orchestration platform in this repository—integrate a narrow executor backend. Add adversarial tests for filesystem escape, host socket access, fork bomb, disk exhaustion, network denial, credential scope, timeout, and cleanup after worker death.
```

## 2.5 Build leases, cancellation, and fairness

Was: 4.3
Status: open
Depends on: 2.1 if build claims move onto the shared durable-work package; otherwise keep the existing build-lease table and converge later.

Prompt:

```text
Evolve the CockroachDB build queue into a durable, fair lease-based scheduler without introducing Temporal or another general workflow system. Builders claim work transactionally with a lease epoch and expiry, heartbeat that lease, and may complete only with the current fencing token. User cancellation and supersession must prevent late completion from publishing an image or starting a rollout. Retry transient worker loss with a bounded attempt count while treating deterministic source/build failures as terminal. Enforce per-workspace and global concurrent-build quotas, weighted fairness between tenants, maximum queue age, build timeout, and admission rejection when account limits are exhausted. Expose queue position approximately, attempt history, cancellation progress, and operator drain controls. Test worker death, split ownership, late completion, cancellation races, starvation resistance, and quota release.
```

## 2.6 Deploy-by-digest and provenance

Was: 4.5
Status: open
Depends on: 1.5 so Railpack and Dockerfile builds both emit the artifact record.

Mutable tags must never be the runtime identity of what is scheduled.

Prompt:

```text
Create an immutable artifact record for every successful build containing source snapshot digest, commit SHA, build recipe and builder version, dependency/build plan where available, image manifest digest, target architecture, build actor, timestamps, and isolation executor identity. Generate an SPDX or CycloneDX SBOM, attach provenance using a standard attestations format, and sign the resulting image digest with a configured signing provider. Deployments must resolve and persist a digest before scheduling; mutable tags are accepted only as user input and never as the runtime identity. Add optional policy gates for unsigned images, failed vulnerability scans, forbidden severity, and stale scans, with clear project-level overrides for authorized roles. Preserve exact artifacts needed for rollback according to retention policy and test tag mutation, signature verification failure, multi-arch selection, and policy enforcement.
```

## 2.7 Highly available convergent ingress

Was: 5.2
Status: open
Depends on: none. One Caddy is currently a product outage.

Prompt:

```text
Replace the single mutable Caddy target assumption with a small fleet of interchangeable ingress instances managed through an IngressProvider contract. Keep Caddy as the first implementation. Each instance must receive a versioned complete routing snapshot derived from CockroachDB, acknowledge the applied revision, retain the last known-good configuration on rejection, and converge after restart or network partition. Control-plane replicas may race to reconcile but must produce the same canonical configuration and must not partially publish a rollout. Readiness should require enough ingress instances at the current revision to satisfy the configured availability policy. Route only ready, non-draining allocations; support multiple replicas; and remove a backend before destructive shutdown. Provide per-instance status, config diff diagnostics without secrets, staged validation, rollback to last known good, and failure tests for one ingress down, invalid config, delayed apply, and split control-plane ownership.
```

## 2.8 Domain and certificate lifecycle

Was: 5.3
Status: open
Depends on: 1.6 so deleted hostnames cannot be rebound during grace.

Prompt:

```text
Turn domain bindings into an explicit verification and certificate lifecycle. Persist requested, verification-pending, verified, certificate-pending, active, degraded, and removing states with safe reason codes and timestamps. Prevent hostname takeover by proving DNS ownership according to the domain type, recheck ownership periodically, and avoid serving a new customer’s workload on a hostname retained from a deleted project. Integrate certificate issuance through Caddy or a narrow ACME provider contract with rate-limit awareness, renewal monitoring, challenge cleanup, and last-known-good certificate behavior. Show exact DNS records, observed values, certificate expiry, and actionable errors in the console. Generated domains must be collision-resistant and reserved transactionally. Test conflicting claims, dangling CNAMEs, rebinding after deletion grace, issuance failure, renewal failure, wildcard restrictions, and certificate expiry alerts.
```

## 2.9 Durable bounded logs

Was: 3.3
Status: open
Depends on: none. The ClickHouse path exists; it must survive outage without blocking reconcile or dropping silently.

Prompt:

```text
Harden the existing ClickHouse log path for multi-tenant production use. Preserve runtime, build, deploy, HTTP, and network log types; add structured attributes for known platform events without parsing arbitrary customer output; define ordering and duplicate handling across reconnects; and retain the raw line exactly within a documented size limit. Agents and builders need bounded disk-backed spooling, batching, backpressure, retry, and explicit dropped-line counters so a ClickHouse outage cannot consume unbounded memory or block workload reconciliation. Enforce per-allocation rate and burst limits, tenant retention policies, authorized time-range search, pagination or streaming, and safe deletion after project expiry. The console should offer environment-wide search and deployment-scoped logs with clear gaps when data was dropped. Test backend outage, retry duplication, oversized lines, abusive log rates, retention, and tenant isolation.
```

## 2.10 Bound desired-state sync

Was: 2.6
Status: open
Depends on: 1.1 because snapshots now carry two addresses per allocation.

Needed before commercial scale, not before the first design partner if the fleet is small. Do not block 2.4–2.8 on this.

Prompt:

```text
Keep agents dumb and the full desired snapshot as the canonical recovery format, but make synchronization safe at commercial scale. Assign a content hash and monotonically increasing revision to each per-node snapshot, compress it on the wire, cap its size, and avoid resending an identical revision after reconnect. Split exceptionally large cluster-wide workload identity catalogs into versioned chunks or a separately hashed section so a single service change does not require unbounded allocation and serialization on every control-plane replica. Agents must assemble and validate a complete revision before applying it, retain the last known-good state across reconnect, reject stale or partial revisions, and report applied revision/hash. The control plane needs sync lag and payload-size metrics plus a fallback full resync when history is unavailable. Preserve fail-closed identity removal semantics and test reconnect storms, missed revisions, corrupt chunks, control-plane failover, catalog growth, and removal convergence.
```

## 2.11 Production-like topology harness

Was: 9.1
Status: open
Depends on: none. Grow the existing testvm as 2.2, 2.4, 2.7, and 2.9 need components. Done when the topology below exists.

Prompt:

```text
Extend the OpenTofu/VM harness into a production-like disposable topology with multiple control-plane replicas, a CockroachDB cluster, VictoriaMetrics, ClickHouse, object storage, registry, at least two ingress instances, builders, and at least three agents across distinct failure domains. Keep external managed-provider behaviors behind test doubles or lightweight compatible services where provisioning the real provider is inappropriate. Generate per-run credentials, expose no admin service publicly, collect sanitized artifacts, and guarantee teardown on success, failure, or interruption. The harness must deploy the actual built binaries and configuration profiles rather than alternate test implementations. Produce a machine-readable topology manifest and health summary so scenario tests can target components deterministically.
```

## 2.12 Core onboarding path

Was: 8.9
Status: open
Depends on: 1.4, 1.5, 1.6 so empty states, builder choice, and deletion grace are real.

This is the human path through the loop, not decorative templates.

Prompt:

```text
Refine the console around the platform’s actual supported path: create or choose a project, connect an authorized GitHub repository or direct image, inspect detected build configuration, create a service, review staged changes, deploy, watch state, and reach a healthy endpoint. Add project/workspace navigation, useful empty states, capacity and quota explanations, and error recovery that preserves user input. Every icon button and field needs an accessible name, dialogs need focus management and keyboard behavior, status cannot depend only on color, timestamps and loading states must be truthful, and destructive actions must state scope and recovery. Provide responsive layouts without hiding required controls. Do not add decorative templates before the core path works. Add focused component tests and a Playwright journey covering keyboard-only onboarding, failed build recovery, deploy, rollback, and deletion grace.
```
