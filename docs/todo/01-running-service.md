# 1 — The running service is real

Until these items land, the product does not actually run a typical app, or it lies about whether it did. Work them before observability polish, billing, PR environments, or stateful storage.

Independent items in this file may proceed in parallel. 1.3 should see both overlay families once 1.1 exists; 1.4 should consume the health states from 1.3.

## 1.1 Dual-stack workload overlay

Was: 5.1
Status: in progress
Depends on: none

Typical images bind `0.0.0.0` and never become healthy or publicly reachable on an IPv6-only overlay.

Prompt:

```text
Add IPv4 as a second overlay family next to the existing IPv6 mesh so every allocated workload gets both addresses, both are routed in WireGuard AllowedIPs, and both are enforced by the eBPF identity policy under the same network_identity. Allocate IPv4 from a real sequential pool and non-overlapping per-node prefixes—do not hash it the IPv6 way—and reject pool exhaustion or overlap transactionally. Leave underlay advertise_addr on IPv6. Extend desired state, allocation reports, container labels, CNI setup, internal host entries, service DNS, health probes, metrics labels, and Caddy backends to understand both addresses and choose a reachable healthy family without weakening environment isolation. A process binding only 0.0.0.0 must become healthy and publicly reachable, and a process binding only :: must continue to work. Isolation tests must prove same-environment allow, cross-environment deny, unknown-destination deny, identity removal, and node failover on both families.
```

## 1.2 Encrypted versioned secrets

Was: 2.4
Status: open
Depends on: none. May introduce envelope keys against the current file material; 2.3 later replaces the provider with KMS/HSM.

A real service cannot run without credentials that never appear in revision JSON, logs, or the console.

Prompt:

```text
Separate public configuration variables from secrets. Persist secrets as versioned ciphertext encrypted with per-project or per-environment data-encryption keys wrapped by the configured production key provider; never include plaintext in service revision JSON, change descriptions, audit payloads, API reads, logs, or console loader data. Service specs should reference secret versions, and only the control-plane path assembling desired state may decrypt the exact versions needed for an assigned workload. Agents may receive runtime plaintext only for their assigned allocations, must write no plaintext desired-state JSON to disk, and must discard it when the allocation is removed. The console must support write-only create/update, masked existence, explicit deletion, and safe copy restrictions. Rollback must restore references to historic secret versions without revealing them. Add key rotation, authorization, redaction, compromise-scope, and no-plaintext-at-rest tests.
```

## 1.3 Continuous startup, readiness, and liveness

Was: 1.2
Status: open
Depends on: 1.1 for probing both overlay families. Restart policy already exists.

Readiness is still a latched rollout gate. A wedged process stays “healthy” and keeps receiving traffic.

Prompt:

```text
Expand the runtime health model from a latched rollout gate into startup, readiness, and liveness checks with explicit semantics. Startup checks may delay liveness enforcement during boot; readiness controls whether an allocation receives ingress; liveness continuously detects a wedged process and can request a restart according to policy. Support HTTP and TCP checks, configurable path/port, interval, timeout, success threshold, failure threshold, and initial delay with bounded safe defaults. Probes must originate inside the persisted workload network namespace, target only the assigned workload address and declared port, refuse redirects and proxy environment variables, and work over both overlay families after dual-stack lands. Report recent probe result, failure reason, counters, and transition time without flooding CockroachDB on every successful probe. Remove the permanent readiness latch and test recovery, intermittent failure, stale namespaces, undeclared ports, and post-readiness failure.
```

## 1.4 Truthful time and status

Was: 0.2
Status: open
Depends on: 1.3 so status labels can distinguish unhealthy from not-yet-ready.

A staged service must not look like it has been running since 1970, and deployment states already persisted must be what the console shows.

Prompt:

```text
Remove impossible timestamps and ambiguous deployment state from the control-plane-to-console path. Define how absent protobuf timestamps are represented, ensure zero/epoch values stay absent rather than becoming JavaScript Date objects, and make every deployment/build/allocation timestamp use UTC at storage and transport boundaries. A newly staged service with no build or rollout time must show an intentional label such as “Not deployed” rather than an age measured from 1970. Consolidate relative-time formatting, handle future clock skew defensively, and ensure status labels distinguish staged, queued, building, deploying, healthy, unhealthy, crashed, superseded, cancelled, and removed states once those states exist. Add codec and component tests for missing, zero, malformed, future, and valid timestamps.
```

## 1.5 Railpack as the default builder

Was: 4.1
Status: open
Depends on: none. Isolation (2.4) can wrap this later; do not wait for it.

Most repositories have no Dockerfile. Silent Dockerfile fallback hides why a source deploy failed.

Prompt:

```text
Make Railpack a first-class field on BuildRecipe and the default for every new inspect, create, and onboarding path, while Dockerfile remains an explicit selectable recipe with dockerfile_path and context_dir. Do it as a clean cutover: builder is required, omitted or unspecified values are rejected, and all newly created repository services choose Railpack unless the user selects Dockerfile. Thread the builder choice through protobufs, JSON recipe encoding, source inspection, staged-change descriptions, console create/update/settings, claimed BuildJob, deployment details, and config-as-code once available. Dispatch in internal/builder so both paths use the same immutable snapshot workspace, exact registry capability, log reporting, cancellation, resource limits, and final digest verification. Railpack analysis failure must produce actionable detected-language and missing-start-command details rather than silently falling back to Dockerfile. Add representative Node, Python, Go, static-site, monorepo, Dockerfile, and no-buildable-source tests.
```

## 1.6 Safe deletion

Was: 0.3
Status: open
Depends on: none. Garbage collection may start as a dedicated loop and move onto the durable-work package (2.1) later.

Prompt:

```text
Replace immediate destructive deletion of projects, environments, services, domains, source archives, and volumes with explicit lifecycle semantics appropriate to each resource. User-facing delete operations must record who requested deletion, stop new work, withdraw ingress, and place recoverable resources into a tombstoned state for a configurable grace period before background garbage collection performs irreversible cleanup. Production-environment and volume deletion must require a typed confirmation tied to the current resource name and must report dependent services and domains before acceptance. Repeated requests and garbage-collector retries must be idempotent. Resource listings should hide tombstones by default but expose them to authorized recovery and operator flows. Do not claim volume recovery until the stateful volume provider provides it; until then, fail closed on deleting an attached or non-empty production volume. Cover concurrent delete/deploy, restore-during-grace, expired cleanup, and partial external-cleanup failures.
```

## 1.7 Credentials out of process arguments

Was: 0.1
Status: open
Depends on: none

Small, independent, and currently leaking the Cloudflare tunnel token through argv.

Prompt:

```text
Eliminate the Cloudflare tunnel token exposure in the local product harness and establish a reusable child-process secret-handling rule. The tunnel credential must never appear in argv, inherited environment diagnostics, structured logs, test artifacts, command error strings, or process startup summaries.
```
