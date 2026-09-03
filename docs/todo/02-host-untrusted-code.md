# 2 — Host untrusted code without a shared disk

These items make the dogfood loop safe to offer a design partner. Control-plane replicas already exist; they still share node-local source and keys. Builds still run as a host process. Ingress is still one Caddy. Agents still get a full-cluster identity catalog over a full WireGuard mesh.

Do not wait to empty this file before starting 3.x items that have no dependency here. Do wait to invite a second tenant’s source onto a shared builder until 2.4 exists.

Extend the VM harness as each item needs a new topology component; 2.13 is done when the listed topology exists.

## 2.1 Durable-work package

Was: 2.7
Status: open
Depends on: none

Prompt:

```text
Create a small CockroachDB-backed durable-work package for control-plane background operations. It is not a workflow DSL. A work record needs a stable kind and ID, deduplication/idempotency key, scoped resource identity, pending/leased/succeeded/failed/dead state, attempt limit, lease owner, monotonically increasing lease epoch used as a fencing token, lease expiry and heartbeat, sanitized last error, and timestamps. Enqueue atomically with the product-state mutation that requires it. Claim atomically, and require the current lease epoch on heartbeat, completion, and every state-changing callback. Handlers persist intent before an external effect, record observation afterward, and reconcile ambiguous outcomes. Provide bounded retry with jitter, dead-letter inspection, and queue-lag metrics. First merge is the package plus one caller (prefer GitHub deliveries or deletion GC). Migrate other loops only when you next touch them. Add multi-replica tests that kill a worker before and after an external-effect boundary and prove a stalled owner cannot commit after takeover.
```

## 2.2 Source object storage

Was: 2.2
Status: open
Depends on: 1.8 for deletion grace. Production must stop requiring a shared filesystem of archives across control-plane replicas.

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

## 2.6 Deploy-by-digest

Was: 4.5
Status: open
Depends on: 1.6 so Railpack and Dockerfile builds both emit the artifact record.

Mutable tags must never be the runtime identity of what is scheduled.

Prompt:

```text
Create an immutable artifact record for every successful build containing source snapshot digest, commit SHA, build recipe and builder version, image manifest digest, target architecture, build actor, and timestamps. Deployments must resolve and persist a digest before scheduling; mutable tags are accepted only as user input and never as the runtime identity. Direct-image deploys resolve the tag to a digest at deploy time and store that digest. Preserve exact artifacts needed for rollback according to retention policy. Test tag mutation after resolve (the stored digest still runs) and multi-arch selection against 1.10. Do not add SBOM generation, image signing, or vulnerability-policy gates in this item.
```

## 2.7 Envoy ingress fleet

Was: 5.2 (Caddy). Clean cutover: Envoy replaces Caddy as production and local-stack ingress.

Status: open
Depends on: none. One Caddy is currently a product outage and the wrong apply protocol.

Prompt:

```text
Replace Caddy with a small fleet of interchangeable Envoy instances. The control plane is the xDS authority: it serves a versioned snapshot (CDS/EDS/LDS/RDS, SDS once certificates exist) derived from CockroachDB. Each Envoy ACK/NACKs the applied revision, retains last-known-good on NACK, and converges after restart or partition. Control-plane replicas may race to compute config but must produce the same canonical snapshot and must not partially publish a rollout. Readiness requires enough Envoy instances at the current revision to satisfy the configured availability policy. Route only ready, non-draining allocations; support multiple replicas; remove a backend from EDS before destructive shutdown. Provide per-instance status and a config diff without secrets. Failure tests: one Envoy down, NACK of a bad snapshot, delayed apply, split control-plane ownership. Remove Caddy from the production path, localteststack, and VM harness. Do not implement Envoy, a WAF, or a CDN in this repository.
```

## 2.8 Domain and certificate lifecycle

Was: 5.3
Status: open
Depends on: 1.8 so deleted hostnames cannot be rebound during grace. 2.7 so certs are pushed to Envoy via SDS rather than Caddy automatic HTTPS.

Prompt:

```text
Turn domain bindings into an explicit verification and certificate lifecycle. Persist requested, verification-pending, verified, certificate-pending, active, degraded, and removing states with safe reason codes and timestamps. Prevent hostname takeover by proving DNS ownership according to the domain type, recheck ownership periodically, and avoid serving a new customer’s workload on a hostname retained from a deleted project. Issue certificates through a narrow ACME provider contract (not Caddy); push materials to Envoy over SDS with rate-limit awareness, renewal monitoring, challenge cleanup, and last-known-good certificate behavior. Show exact DNS records, observed values, certificate expiry, and actionable errors in the console. Generated domains must be collision-resistant and reserved transactionally. Test conflicting claims, dangling CNAMEs, rebinding after deletion grace, issuance failure, renewal failure, and certificate expiry alerts.
```

## 2.9 Durable bounded logs

Was: 3.3
Status: open
Depends on: none

Prompt:

```text
Harden the existing ClickHouse log path for multi-tenant production use. Preserve runtime, build, deploy, HTTP, and network log types; add structured attributes for known platform events without parsing arbitrary customer output; define ordering and duplicate handling across reconnects; and retain the raw line exactly within a documented size limit. Agents and builders need bounded disk-backed spooling, batching, backpressure, retry, and explicit dropped-line counters so a ClickHouse outage cannot consume unbounded memory or block workload reconciliation. Enforce per-allocation rate and burst limits, tenant retention policies, authorized time-range search, pagination or streaming, and safe deletion after project expiry. The console should offer environment-wide search and deployment-scoped logs with clear gaps when data was dropped. Test backend outage, retry duplication, oversized lines, abusive log rates, retention, and tenant isolation.
```

## 2.10 Nomad-style per-node allocation sync

Status: open
Depends on: none. Replaces the full desired-state snapshot as the agent wire and recovery format.

Nomad servers send each client only that client’s allocations. Reconnect is “what I run” reconciled against “what this node should run,” not a cluster dump.

Prompt:

```text
Stop sending each agent a full cluster desired-state snapshot. The control plane remains authoritative for placement. The agent stream carries only allocations assigned to that agent: start, update, and stop diffs, each with a monotonically increasing per-node revision. The agent persists its local alloc set, reports observed state, and on reconnect sends the alloc IDs and versions it is running. The control plane replies with the desired set for that node; the agent starts missing allocs, stops extras, and must not restart a healthy alloc whose desired spec and generation still match. Unknown local containers are stopped (fail closed). Other nodes’ allocations never appear as “run this.” Identity/policy (2.11) and WireGuard peers (2.12) are separate messages, not stuffed into the alloc body. Cap payload size per revision and skip a resend when the agent already has that per-node revision. Test reconnect without container restart, missed stop, control-plane failover mid-sync, and a node that comes back with extra containers.
```

## 2.11 Scoped identity policy

Status: open
Depends on: 2.10 so policy is not piggy-backed on a cluster snapshot. 1.1 if both overlay families are present.

The fail-closed pool deny stays. The cluster-wide identity catalog on every node does not.

Prompt:

```text
Give each agent a pool deny plus exact allow entries only for network identities in environments that agent currently hosts, including remote allocations in those environments so same-environment east-west still works. Do not send identities for environments the node does not host. Envoy instances receive the backends they route, not the mesh catalog. Adding or removing an allocation updates only agents in that environment and the Envoy instances that publish it. Identity removal must still fail closed: unknown destinations and cross-environment traffic are denied on both overlay families. Test that a node hosting only environment A has no environment-B allows, that growth in B does not change A’s agents, that same-environment cross-node traffic still works, and that removing the last A allocation on a node drops A’s allows.
```

## 2.12 Environment-scoped WireGuard peering

Status: open
Depends on: 2.11 so peers follow who is allowed to talk. 2.7 so ingress instances are the north-south peers.

Full mesh does not scale and is not required for fail-closed policy.

Prompt:

```text
Stop forming a WireGuard peer between every pair of agents. Create and maintain tunnels only (1) between agents that currently share at least one environment and (2) between those agents and the Envoy instances that publish their allocations. AllowedIPs on each peer are only that peer’s overlay prefixes, not the whole cluster. When the last shared environment leaves a pair of nodes, tear the peer down. Control-plane mTLS stays off the mesh. Underlay advertise_addr remains IPv6. Test: disjoint environments produce no agent-agent peer; adding a shared environment creates a peer; removing it destroys the peer; east-west same-environment still works; cross-environment stays denied without a tunnel; Envoy can still reach ready backends.
```

## 2.13 Production-like topology harness

Was: 9.1
Status: open
Depends on: none. Grow the existing testvm as 2.2, 2.4, 2.7, and 2.9 need components. Done when the topology below exists.

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
