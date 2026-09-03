# Already landed

These prompts are done. They stay here so the work queue in [README.md](README.md) only lists open product-priority work. Old IDs are in parentheses.

Current code still uses a full-cluster identity catalog, a full WireGuard mesh, a full desired-state snapshot on every agent, and a single Caddy. Those are not the architecture to preserve; [2.7](02-host-untrusted-code.md#27-envoy-ingress-fleet)–[2.12](02-host-untrusted-code.md#212-environment-scoped-wireguard-peering) replace them.

## Production configuration profile (0.4)

Fail-closed production startup, separate liveness and readiness, sanitized startup contract, explicit development profile.

## Authoritative deployment state machine (1.1)

Persisted lifecycle shared by builds, rollouts, allocations, ingress, and the console, with legal transitions in CockroachDB.

## Restart policies and crash-loop control (1.3)

Explicit always / on-failure / never policy, backoff, durable observation, crash-loop terminal state.

## Multiple allocations per stateless service (1.4)

Desired replica count, independent allocation identities, scheduler placement, scale controls.

## Zero-downtime rolling replacement (1.5)

New allocations alongside the serving generation, ingress switch, graceful drain. Volume-backed services still reject overlap until [6.4](06-stateful.md#64-fenced-stateful-failover).

## Deployment actions (1.6)

Restart, exact redeploy, rollback, cancel, remove, retry. Audit events wait for [3.4](03-operate-multi-tenant.md#34-audit-history).

## Agent fleet lifecycle (1.7)

Enroll / cordon / drain / retire, failure-domain placement, credential revocation. Stateful drain waits for [6.4](06-stateful.md#64-fenced-stateful-failover).

## Production OCI sandbox (1.8)

Single untrusted-workload sandbox, cgroup isolation, no customer-selectable privileged profile. Ephemeral disk cap is [1.11](01-running-service.md#111-bounded-ephemeral-disk).

## Multi-replica control plane (2.1)

Horizontally runnable `cmd/controlplane` with fenced CockroachDB leases. Replicas still share node-local source archives and keys until [2.2](02-host-untrusted-code.md#22-source-object-storage) and [2.3](02-host-untrusted-code.md#23-externalize-platform-keys).
