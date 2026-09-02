# Stage 1 — Workload Lifecycle and Deployments

This stage makes stateless service behavior predictable under deploys, crashes, and node failures.

## 1.1 Define one authoritative deployment and allocation state machine

Prompt:

```text
Replace inferred deployment status with an explicit persisted state machine shared by builds, rollouts, allocations, ingress publication, and the console. Model the complete lifecycle from staged configuration through queued build, build, scheduling, image pull, startup, readiness, active service, draining, completion, failure, cancellation, crash, removal, and supersession. Record state transition time, actor or system cause, machine-readable reason code, safe human detail, target spec revision, image digest, and rollout generation. Enforce legal transitions transactionally in CockroachDB and make retries idempotent. Agent status may advance allocation observations but must not be able to resurrect a terminal deployment. Derive console presentation and deployment history from this model rather than combining nullable build and allocation fields ad hoc. Add table-driven transition tests and concurrency tests for webhook, user action, builder completion, and agent report races.
```

## 1.2 Separate startup, readiness, and continuous liveness health

Prompt:

```text
Expand the runtime health model from a latched rollout gate into startup, readiness, and liveness checks with explicit semantics. Startup checks may delay liveness enforcement during boot; readiness controls whether an allocation receives ingress; liveness continuously detects a wedged process and can request a restart according to policy. Support HTTP and TCP checks, configurable path/port, interval, timeout, success threshold, failure threshold, and initial delay with bounded safe defaults. Probes must originate inside the persisted workload network namespace, target only the assigned workload address and declared port, refuse redirects and proxy environment variables, and work over both overlay families after dual-stack lands. Report recent probe result, failure reason, counters, and transition time without flooding CockroachDB on every successful probe. Remove the permanent readiness latch and test recovery, intermittent failure, stale namespaces, undeclared ports, and post-readiness failure.
```

## 1.3 Add restart policies and crash-loop control

Prompt:

```text
Make process restart behavior an explicit service setting instead of an accidental consequence of reconciliation. Support always, on-failure, and never policies; a bounded retry count or retry window; exponential backoff with jitter and a maximum delay; and reset of crash history after a stable run. Persist enough restart observation to survive agent and control-plane restarts, while keeping the agent responsible for prompt local enforcement. Distinguish exit code zero, non-zero exit, signal, OOM kill, liveness restart, operator restart, and node loss. Once the retry budget is exhausted, leave the allocation in a visible crash-loop terminal state, withdraw it from ingress, emit an event, and require an authorized action or new rollout to retry. Ensure reconciliation does not spin or recreate containers indefinitely, and add deterministic tests using an injectable clock.
```

## 1.4 Support multiple allocations per stateless service

Prompt:

```text
Change the service model from one unique allocation to a desired replica count with independent allocation identities. Let each allocation have its own agent, private addresses, lifecycle state, health, restart history, and rollout generation while the service keeps a stable internal DNS name and public domains. Extend the scheduler to place replicas transactionally by available CPU and memory, avoid co-locating replicas when healthy capacity exists, exclude reserved or unhealthy agents, and explain pending capacity failures. Desired state must contain only allocations assigned to that agent while the identity catalog still distributes the cluster-wide identities needed by the eBPF policy. Ingress and internal DNS must publish only ready allocations and distribute traffic across them. Provide scale up/down controls with safe minimums and production confirmation when scaling to zero. Test partial health, node loss, concurrent scaling, capacity exhaustion, and deterministic placement.
```

# ---- HERE  ----

## 1.5 Implement zero-downtime rolling replacement and graceful draining

Prompt:

```text
Implement rolling deployment as new allocations alongside the currently active generation instead of deleting the old container first. A configurable strategy must bound unavailable and surge allocations, wait for startup/readiness, switch only healthy backends into ingress, then mark the old generation draining. Send SIGTERM, stop routing new connections, allow a configurable drain deadline, and use SIGKILL only after that deadline. A failed replacement must leave the last healthy generation serving and mark the rollout failed with a useful reason. Volume-backed single-writer services must reject unsafe overlap until Stage 7 supplies an appropriate handoff protocol. Make rollout progress durable and recoverable after control-plane or agent restart. Test single-replica overlap, multi-replica batches, readiness failure, shutdown timeout, node loss mid-rollout, and ingress convergence.
```

## 1.6 Add complete deployment actions

Prompt:

```text
Add authorized restart, exact redeploy, rollback, cancel, remove, and retry actions to the platform API and console. Exact redeploy reuses the selected deployment’s immutable image digest and resolved configuration without fetching newer source. Rollback creates a new rollout from a selected previously successful spec and image, including the variable versions that were active for that deployment, while preserving history. Cancel stops queued/building/deploying work cooperatively and cannot be overwritten by a late builder or agent response. Restart affects the selected active allocation or all replicas without creating a new build. Remove withdraws and drains the active deployment while retaining its history. Every action must be idempotent, role-authorized, audited once Stage 6 lands, visible in deployment history, and covered for stale IDs and concurrent actions.
```


## 1.7 Add agent fleet lifecycle and failure-domain-aware capacity

Prompt:

```text
Make compute nodes an operator-managed fleet with explicit enrolling, active, cordoned, draining, unavailable, and retired states. Persist operator-defined region, zone or failure-domain labels, schedulable resource reservations, supported runtime capabilities, software version, and last heartbeat; never trust customer workloads to supply these labels. New placement must avoid cordoned nodes, spread replicas across failure domains when capacity permits, respect service region constraints, and produce a useful pending explanation when it cannot. Draining migrates stateless allocations through the normal rollout/failover machinery and coordinates fenced stateful handoff once Stage 7 exists. Retirement must revoke node credentials and remove mesh identities only after allocations and attachments are gone. Add capacity/headroom views, safe maintenance controls, version-skew warnings, and tests for drain interruption, node return, insufficient alternate capacity, replica spreading, and credential revocation.
```

## 1.8 Harden customer workload isolation

Prompt:

```text
Define and enforce one production OCI sandbox for every untrusted workload. Preserve the image USER, use a writable ephemeral overlay root, and retain only the bounded capabilities needed by ordinary 12-factor images (`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETGID`, `SETUID`, `SETPCAP`, `NET_BIND_SERVICE`, and `KILL`). Also enforce `no_new_privileges`, PID and process limits, a maintained seccomp profile, AppArmor or SELinux when already supported by the host, no host devices or sockets, isolated IPC/UTS/network/mount/cgroup namespaces, masked dangerous proc/sys paths, and no privileged, host-network, host-PID, arbitrary sysctl, device, bind-mount, or alternate sandbox-profile configuration through the customer API. Enforce hard cgroup v2 CPU, memory, swap, and OOM behavior and reserve resources for the agent/runtime so tenant pressure cannot kill reconciliation. Add adversarial VM tests for host filesystem/socket access, process exhaustion, memory pressure, namespace escape prerequisites, device access, and noisy-neighbor containment.
```
