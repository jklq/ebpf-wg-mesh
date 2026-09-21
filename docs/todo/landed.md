# Already landed

These prompts are done. They stay here so the work queue in [README.md](README.md) only lists open product-priority work. Old IDs are in parentheses.

Current code still uses an environment-scoped identity catalog, environment-scoped WireGuard peering, a full desired-state snapshot on every agent, and a single Caddy. The catalog, per-node sync, and ingress shapes are not the architecture to preserve; [2.7a](02-host-untrusted-code.md#27a-xds-control-plane-and-caddy-cutover), [2.10](02-host-untrusted-code.md#210-incremental-per-node-allocation-sync), and [2.11](02-host-untrusted-code.md#211-scoped-identity-policy)–[2.12](02-host-untrusted-code.md#212-environment-scoped-wireguard-peering) finish replacing them.

## Production configuration profile (0.4)

Fail-closed production startup, separate liveness and readiness, sanitized startup contract, explicit development profile.

## Authoritative deployment state machine (1.1)

Persisted lifecycle shared by builds, rollouts, allocations, ingress, and the console, with legal transitions in CockroachDB.

## Restart policies and crash-loop control (1.3)

Explicit always / on-failure / never policy, backoff, durable observation, crash-loop terminal state.

## Multiple allocations per stateless service (1.4)

Desired replica count, independent allocation identities, scheduler placement, scale controls.

## Zero-downtime rolling replacement (1.5)

New allocations alongside the serving generation, ingress switch, graceful drain. Volume-backed services reject overlap: a volume redeploy takes downtime, and replicas cannot be used with volumes.

## Deployment actions (1.6)

Restart, exact redeploy, rollback, cancel, remove, retry. Audit events wait for [3.4](03-operate-multi-tenant.md#34-audit-history).

## Agent fleet lifecycle (1.7)

Enroll / cordon / drain / retire, failure-domain placement, credential revocation. Volume-backed allocations stay on their node through drain; they are not failed over to another node.

## Production OCI sandbox (1.8)

Single untrusted-workload sandbox, cgroup isolation, no customer-selectable privileged profile. Ephemeral disk is capped at 1 GiB per allocation (1.11 below).

## Multi-replica control plane (2.1)

Horizontally runnable `cmd/controlplane` with fenced CockroachDB leases, a durable product-state journal and indexed live views. Agents persist replica discovery, follow live-owner redirects, quarantine a failed owner briefly, and reconnect after fenced takeover. Production source archives live in S3-compatible object storage since [2.2](#22-source-object-storage); sealed-secret keys landed in [2.3a](#23a-secret-envelope-key-provider); replicas still share node-local signing keys until [2.3b](02-host-untrusted-code.md#23b-platform-signing-key-lifecycle).

## Durable agent reconciliation foundation

Agents durably stage and accept complete agent-scoped snapshots under expiring, epoch-fenced authority. They retain allocation generations, runtime identities, pending operations, drains, and observation order; supervise accepted work while disconnected; reconcile discovered runtime resources before reconnecting; quarantine corrupt state; and require authoritative ownership before destructive recovery. Incremental delivery remains [2.10](02-host-untrusted-code.md#210-incremental-per-node-allocation-sync).

## 1.1 Dual-stack workload overlay

Was: 5.1
Status: done
Depends on: none

Typical images bind `0.0.0.0` and never become healthy or publicly reachable on an IPv6-only overlay.

Prompt:

```text
Add IPv4 as a second overlay family next to the existing IPv6 mesh so every allocated workload gets both addresses, both are routed in WireGuard AllowedIPs, and both are enforced by the eBPF identity policy under the same network_identity. Allocate IPv4 from a real sequential pool and non-overlapping per-node prefixes—do not hash it the IPv6 way—and reject pool exhaustion or overlap transactionally. Keep the host-identity `advertise_addr` on IPv6; the WireGuard transport endpoint is independently advertised and may use IPv4 or IPv6. Extend desired state, allocation reports, container labels, CNI setup, internal host entries, service DNS, health probes, metrics labels, and ingress backends to understand both addresses and choose a reachable healthy family without weakening environment isolation. A process binding only 0.0.0.0 must become healthy and publicly reachable, and a process binding only :: must continue to work. Isolation tests must prove same-environment allow, cross-environment deny, unknown-destination deny, identity removal, and node failover on both families.
```

## 1.4 Crash evidence in the console

Status: done
Depends on: 1.3 so probe failures are distinct from process death. Restart observation already exists.

Without last exit, OOM, and leftover logs, 1.3 is invisible.

Prompt:

```text
Surface crash evidence on the service and allocation views from persisted restart observation and bounded recent logs. Show last exit code, signal, OOM kill, liveness failure, restart count, crash-loop, and a short tail of runtime logs that remains after the container is gone. Distinguish OOM, liveness restart, nonzero exit, and probe-not-ready. Do not require a new log backend; use the existing ClickHouse path and durable observation already stored for restart policy. Test that a crash-looped allocation remains diagnosable after the container has been removed.
```

## 1.5 Truthful time and status

Was: 0.2
Status: done
Depends on: restart observation so status labels can distinguish crashed from not-yet-ready.

Prompt:

```text
Remove impossible timestamps and ambiguous deployment state from the control-plane-to-console path. Define how absent protobuf timestamps are represented, ensure zero/epoch values stay absent rather than becoming JavaScript Date objects, and make every deployment/build/allocation timestamp use UTC at storage and transport boundaries. A newly staged service with no build or rollout time must show an intentional label such as “Not deployed” rather than an age measured from 1970. Consolidate relative-time formatting, handle future clock skew defensively, and ensure status labels distinguish staged, queued, building, deploying, healthy, unhealthy, crashed, superseded, cancelled, and removed states once those states exist. Add codec and component tests for missing, zero, malformed, future, and valid timestamps.
```

## 1.6 Railpack as the default builder

Was: 4.1
Status: done
Depends on: none. The build executor boundary (2.4a) can wrap this later.

Most repositories have no Dockerfile. Silent Dockerfile fallback hides why a source deploy failed.

Prompt:

```text
Make Railpack a first-class field on BuildRecipe and the default for every new inspect, create, and onboarding path, while Dockerfile remains an explicit selectable recipe with dockerfile_path and context_dir. Do it as a clean cutover: builder is required, omitted or unspecified values are rejected, and all newly created repository services choose Railpack unless the user selects Dockerfile. Thread the builder choice through protobufs, JSON recipe encoding, source inspection, staged-change descriptions, console create/update/settings, claimed BuildJob, and deployment details. Dispatch in internal/builder so both paths use the same immutable snapshot workspace, exact registry capability, log reporting, cancellation, resource limits, and final digest verification. Railpack analysis failure must produce actionable detected-language and missing-start-command details rather than silently falling back to Dockerfile. Add representative Node, Python, Go, static-site, monorepo, Dockerfile, and no-buildable-source tests.
```

## 1.7 Auto-deploy on/off per environment

Status: done
Depends on: none. Webhooks already queue work.

Partners need production to stay manual.

Prompt:

```text
Add an explicit auto-deploy setting on each environment. When on, a verified push to a service’s tracked ref queues a deploy. When off, the control plane records the new source revision and does not start a build or rollout until an authorized Deploy/Retry. Default on for all environments; the setting is overridable. Manual deploy always works. The console must show whether the latest commit is deployed, waiting, or ignored because auto-deploy is off. Test webhook delivery with the flag on and off and a later manual deploy of the recorded revision.
```

## 1.9 Credentials out of process arguments

Was: 0.1
Status: done
Depends on: none

Prompt:

```text
Eliminate the Cloudflare tunnel token exposure in the local product harness and establish a reusable child-process secret-handling rule. The tunnel credential must never appear in argv, inherited environment diagnostics, structured logs, test artifacts, command error strings, or process startup summaries.
```

## 1.8 Safe deletion

Was: 0.3
Status: done
Depends on: none. Garbage collection runs as a dedicated loop; it may move onto the durable-work package (2.1) later.

Prompt:

```text
Replace immediate destructive deletion of projects, environments, services, domains, source archives, and volumes with explicit lifecycle semantics appropriate to each resource. User-facing delete operations must record who requested deletion, stop new work, withdraw ingress, and place recoverable resources into a tombstoned state for a configurable grace period before background garbage collection performs irreversible cleanup. Production-environment and volume deletion must require a typed confirmation tied to the current resource name and must report dependent services and domains before acceptance. Repeated requests and garbage-collector retries must be idempotent. Resource listings should hide tombstones by default but expose them to authorized recovery and operator flows. Do not claim volume recovery until the stateful volume provider provides it; until then, fail closed on deleting an attached or non-empty production volume. Cover concurrent delete/deploy, restore-during-grace, expired cleanup, and partial external-cleanup failures.
```

## 1.11 Bounded ephemeral disk

Status: done
Depends on: none. The production sandbox already exists.

Overlay writes can fill the node.

Prompt:

```text
Cap each allocation’s ephemeral writable overlay at 1 GiB by default using cgroup v2 I/O or filesystem quota on the production sandbox. When the cap is hit, the allocation must fail with a visible disk-full cause rather than filling the host. The cap is platform policy, not a customer API field, until a later quota item exists. Test that a workload writing past 1 GiB is stopped, that the host disk is not exhausted, and that the console/allocation status names disk exhaustion.
```

## 2.2 Source object storage

Was: 2.2
Status: done
Depends on: 1.8 for deletion grace (objects are collected only when unreferenced and older than the retention grace; full tombstone lifecycle stays with 1.8).

Design-partner minimum. Production control-plane replicas no longer share a filesystem of source archives.

Prompt:

```text
Keep FileSourceArchiveStore for local development and add a production SourceArchiveStore backed by operator-provided S3-compatible object storage. Store content-addressed immutable archives by verified SHA-256 digest, stream uploads and downloads without loading the whole archive into memory, verify size and digest at both boundaries, and use conditional creation so duplicate snapshots converge safely. Persist object metadata and lifecycle state in CockroachDB, distinguish missing/corrupt/transient retrieval failures, and garbage-collect only objects that are unreferenced after the deletion grace period. Support configurable server-side encryption, endpoint, region, bucket, credential-file or workload-identity auth, timeouts, and bounded retries without logging credentials. Production mode must reject the filesystem provider. Add contract tests shared by file and S3-compatible implementations plus failure tests for partial upload, stale metadata, range reads, deletion races, and digest mismatch.
```

## 2.3a Secret envelope key provider

Was: 2.3 (split)
Status: done
Depends on: 1.2 if it lands first; otherwise this item is where secret ciphertext first gets a real key. 1.2 stayed parked, so this item owns the envelope key and the sealed-secret backend; console seal/masked/delete UX remains deferred to [2.3a frontend handoff](../frontend-handoff/2.3a.md) and the future 1.2 unpark.

Secrets are the only key material here whose ciphertext outlives the process. That is what justifies a key manager. Approved design (supersedes the earlier production-KMS draft, which never merged): a small in-process key manager, no AWS KMS, OpenBao, Vault, or external KMS service.

Prompt:

```text
Give secret material a real in-process key manager so ciphertext is not protected by a key that exists only under CONTROLPLANE_STATE_DIR on one replica. Define one KeyProvider contract that wraps and unwraps data-encryption keys without exporting root key material into logs, database rows, process arguments, or error strings. Use established cryptographic libraries: randomly generated data-encryption keys encrypt secret values, and authenticated encryption under versioned master keys wraps those DEKs, with secure random nonces and authenticated record/purpose context that prevents substitution. Provision the same master-key ring to every control-plane replica through an access-restricted file, separately from the database; production allows this explicitly provisioned keyring and never generates missing production keys or falls back to node-local generated keys. Validate key sizes, IDs, file access, and configuration. CockroachDB stores ciphertext, wrapped DEKs, and shared active/retired key-version metadata. Rotation is explicit: provision the new version to all replicas before activation, activate for new writes, rewrap DEKs safely and resumably, and retain previous versions until no reference remains. Detect missing or inconsistent keys and fail closed; refuse to delete or disable a key while ciphertext wrapped by it still exists. No manual unlock during ordinary restart. Document master-key backups, DB/keyring restore, replica provisioning, activation, and retirement. Do not add an HSM or external-signer contract, a compromise-response program, or customer-facing key management in this item. Test multi-replica decrypt and rewrap, wrong/missing keys, tampered ciphertext and context, interrupted rotation and restart, and refusal to delete a key with live ciphertext.
```

Landed as: `KeyProvider` contract (`internal/controlplane/secretkeys`) with a single in-process `keyring` provider (AES-256-GCM from the standard library, no external KMS); a provisioned keyring file replicated to every replica plus a CockroachDB-backed key registry (single active key, retired keys unwrap) and per-environment DEKs; sealed service-secret versions with deployment pins and desired-state decryption on the control plane; `controlplane keys` operator CLI (list/provision/activate/rewrap/delete/check). There is no separate disable state: deleting a retired, unreferenced key is the removal path, and deletion of the active key or a key with live wrapped DEKs is refused. Operator runbook: [secret keyring](../secret-keyring.md).
