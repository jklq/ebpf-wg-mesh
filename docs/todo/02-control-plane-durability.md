# Stage 2 — Control-Plane Durability and Secrets

This stage removes single-process and node-local authority from the production control path without replacing the platform’s CockroachDB coordination model.

## 2.1 Run multiple control-plane replicas safely

Prompt:

```text
Make cmd/controlplane horizontally runnable with at least three interchangeable replicas behind an operator-provided load balancer. Keep product state and coordination in CockroachDB; do not introduce a general workflow engine. Convert every background reconciler, expiry timer, GitHub work consumer, build lease repair loop, ingress sync, garbage collector, and managed-dashboard reconciler to a transactionally claimed or lease-owned unit of work with fencing, bounded retries, jitter, and takeover after owner loss. RPCs and webhook ingestion must remain idempotent when routed to different replicas. In-memory notifications may accelerate work but correctness must come from durable indexes and periodic reconciliation. Expose per-replica readiness, ownership, queue lag, and last-success metrics. Add a multi-replica integration harness that kills the active owner during representative jobs and proves exactly-once effects where required and safe at-least-once execution everywhere else.
```

## 2.2 Move source snapshots to production object storage

Prompt:

```text
Keep FileSourceArchiveStore for local development and add a production SourceArchiveStore backed by operator-provided S3-compatible object storage. Store content-addressed immutable archives by verified SHA-256 digest, stream uploads and downloads without loading the whole archive into memory, verify size and digest at both boundaries, and use conditional creation so duplicate snapshots converge safely. Persist object metadata and lifecycle state in CockroachDB, distinguish missing/corrupt/transient retrieval failures, and garbage-collect only objects that are unreferenced after the deletion grace period. Support configurable server-side encryption, endpoint, region, bucket, credential-file or workload-identity auth, timeouts, and bounded retries without logging credentials. Production mode must reject the filesystem provider. Add contract tests shared by file and S3-compatible implementations plus failure tests for partial upload, stale metadata, range reads, deletion races, and digest mismatch.
```

## 2.3 Externalize and rotate platform key material

Prompt:

```text
Replace production dependence on private keys generated under CONTROLPLANE_STATE_DIR with explicit key providers. Cover the internal CA, registry token signer, user-assertion verifier material, GitHub token encryption, session signing, and any envelope-encryption keys introduced for customer secrets. Support a local file provider for development and a production KMS/HSM or externally mounted signer contract that can sign or unwrap without exporting a long-lived root key into application logs or database rows. Model key IDs and active/retiring/retired states, allow overlapping verification during rotation, make new signatures use the active key, and provide operator commands and runbooks for scheduled rotation and compromise response. Control-plane replicas must see consistent key state. Refuse unsafe key deletion while dependent certificates, tokens, or ciphertext remain valid, and test rotation across live sessions, agent renewal, registry pulls, and replica restarts.
```

## 2.4 Store customer secrets as encrypted versioned values

Prompt:

```text
Separate public configuration variables from secrets. Persist secrets as versioned ciphertext encrypted with per-project or per-environment data-encryption keys wrapped by the configured production key provider; never include plaintext in service revision JSON, change descriptions, audit payloads, API reads, logs, or console loader data. Service specs should reference secret versions, and only the control-plane path assembling desired state may decrypt the exact versions needed for an assigned workload. Agents may receive runtime plaintext only for their assigned allocations, must write no plaintext desired-state JSON to disk, and must discard it when the allocation is removed. The console must support write-only create/update, masked existence, explicit deletion, and safe copy restrictions. Rollback must restore references to historic secret versions without revealing them. Add key rotation, authorization, redaction, compromise-scope, and no-plaintext-at-rest tests.
```

## 2.5 Back up and restore platform authority

Prompt:

```text
Define and implement a coherent backup and restore procedure for CockroachDB platform schemas, console schemas, object-store metadata and archives, PKI/KMS references, registry trust, and required operator configuration. Backups must be automated, encrypted, retained on a documented schedule, monitored for freshness, and restorable into an isolated recovery environment without contacting production agents or mutating production ingress. Provide a restore command or documented orchestration that validates schema versions, referential integrity, source object availability, key access, and registry trust before enabling reconciler side effects. Establish explicit RPO and RTO targets and make the local/VM harness exercise a small backup-mutate-restore scenario. Restoration success means a service’s project, configuration, source snapshot, image reference, domains, deployment history, membership, and audit history are coherent—not merely that database tables can be imported.
```

## 2.6 Bound and recover desired-state synchronization

Prompt:

```text
Keep agents dumb and the full desired snapshot as the canonical recovery format, but make synchronization safe at commercial scale. Assign a content hash and monotonically increasing revision to each per-node snapshot, compress it on the wire, cap its size, and avoid resending an identical revision after reconnect. Split exceptionally large cluster-wide workload identity catalogs into versioned chunks or a separately hashed section so a single service change does not require unbounded allocation and serialization on every control-plane replica. Agents must assemble and validate a complete revision before applying it, retain the last known-good state across reconnect, reject stale or partial revisions, and report applied revision/hash. The control plane needs sync lag and payload-size metrics plus a fallback full resync when history is unavailable. Preserve fail-closed identity removal semantics and test reconnect storms, missed revisions, corrupt chunks, control-plane failover, catalog growth, and removal convergence.
```

## 2.7 Standardize durable work without a workflow engine

Prompt:

```text
Create a small CockroachDB-backed durable-work package used by control-plane background operations instead of allowing each reconciler to invent subtly different claiming behavior. It is not a workflow DSL or general orchestration product. A work record needs a stable kind and ID, schema version, deduplication/idempotency key, scoped resource identity, pending/leased/succeeded/failed/dead state, available time, attempt count and limit, lease owner, monotonically increasing lease epoch used as a fencing token, lease expiry and heartbeat, sanitized last error, result metadata, and created/updated/completed times. Enqueue work atomically with the product-state mutation or transactional outbox that requires it. Claim atomically, and require the current lease epoch on heartbeat, completion, release, cancellation, and every state-changing callback so a stalled worker cannot commit after takeover. Handlers must make external effects idempotent with provider request keys where available, persist intent before performing an effect, record observation afterward, and reconcile ambiguous outcomes instead of assuming exactly-once delivery. Provide bounded exponential retry with jitter, terminal classification, dead-letter inspection/replay, queue lag and attempt metrics, operator drain controls, and cleanup/retention. Migrate GitHub deliveries and source work, build claims, notifications/webhooks, ingress publication, deletion garbage collection, backup/restore operations, billing rollups, scheduled jobs, and other long-running operations onto these primitives where their domain state machine does not require a more specific table. Add multi-replica tests that kill workers before and after each external-effect boundary, force lease expiry and stale completion, replay enqueue requests, and prove eventual convergence without duplicate product effects.
```
