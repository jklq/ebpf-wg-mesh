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

New allocations alongside the serving generation, ingress switch, graceful drain. Volume-backed services still reject overlap until [6.4](06-stateful.md#64-fenced-stateful-failover).

## Deployment actions (1.6)

Restart, exact redeploy, rollback, cancel, remove, retry. Audit events wait for [3.4](03-operate-multi-tenant.md#34-audit-history).

## Agent fleet lifecycle (1.7)

Enroll / cordon / drain / retire, failure-domain placement, credential revocation. Stateful drain waits for [6.4](06-stateful.md#64-fenced-stateful-failover).

## Production OCI sandbox (1.8)

Single untrusted-workload sandbox, cgroup isolation, no customer-selectable privileged profile. Ephemeral disk cap is [1.11](01-running-service.md#111-bounded-ephemeral-disk).

## Multi-replica control plane (2.1)

Horizontally runnable `cmd/controlplane` with fenced CockroachDB leases, a durable product-state journal and indexed live views. Agents persist replica discovery, follow live-owner redirects, quarantine a failed owner briefly, and reconnect after fenced takeover. Replicas still share node-local source archives and keys until [2.2](02-host-untrusted-code.md#22-source-object-storage), [2.3a](02-host-untrusted-code.md#23a-secret-envelope-key-provider), and [2.3b](02-host-untrusted-code.md#23b-platform-signing-key-lifecycle).

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

## 1.5 Truthful time and status

Was: 0.2
Status: done
Depends on: 1.3 so status labels can distinguish unhealthy from not-yet-ready.

Prompt:

```text
Remove impossible timestamps and ambiguous deployment state from the control-plane-to-console path. Define how absent protobuf timestamps are represented, ensure zero/epoch values stay absent rather than becoming JavaScript Date objects, and make every deployment/build/allocation timestamp use UTC at storage and transport boundaries. A newly staged service with no build or rollout time must show an intentional label such as “Not deployed” rather than an age measured from 1970. Consolidate relative-time formatting, handle future clock skew defensively, and ensure status labels distinguish staged, queued, building, deploying, healthy, unhealthy, crashed, superseded, cancelled, and removed states once those states exist. Add codec and component tests for missing, zero, malformed, future, and valid timestamps.
```
