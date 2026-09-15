# 1 — The running service is real

Until these items land, the product does not actually run a typical app, or it lies about whether it did.

Independent items in this file may proceed in parallel. 1.1 is done; see [landed.md](landed.md). 1.2 is parked; see [freeze.md](freeze.md). 1.3 sees both overlay families because 1.1 landed.



## 1.3 Continuous readiness and liveness

Was: 1.2
Status: open
Depends on: 1.1 for probing both overlay families. Restart policy already exists.

Readiness is still a latched rollout gate. The existing HTTP liveness loop can restart a wedged process, but a post-rollout readiness failure alone does not withdraw ingress and the allocation stays “healthy.”

Prompt:

```text
Replace the latched rollout readiness gate with continuous HTTP readiness, using the existing HTTP liveness and restart-policy path rather than creating a second probe system. Readiness controls whether an allocation receives ingress; liveness requests a restart for a wedged process. Support path, port, interval, timeout, and initial delay with bounded safe defaults. Probes must originate inside the persisted workload network namespace, target only the assigned workload address and declared port, refuse redirects and proxy environment variables, and work over both overlay families. Report recent readiness and liveness results, failure reasons, and transition times without writing CockroachDB on every successful tick. Remove the permanent readiness latch. Test recovery, intermittent failure, stale namespaces, undeclared ports, and post-readiness failure. Do not add startup probes, TCP probes, or Kubernetes-style success/failure thresholds.
```

## 1.4 Crash evidence in the console

Status: open
Depends on: 1.3 so probe failures are distinct from process death. Restart observation already exists.

Without last exit, OOM, and leftover logs, 1.3 is invisible.

Prompt:

```text
Surface crash evidence on the service and allocation views from persisted restart observation and bounded recent logs. Show last exit code, signal, OOM kill, liveness failure, restart count, crash-loop, and a short tail of runtime logs that remains after the container is gone. Distinguish OOM, liveness restart, nonzero exit, and probe-not-ready. Do not require a new log backend; use the existing ClickHouse path and durable observation already stored for restart policy. Test that a crash-looped allocation remains diagnosable after the container has been removed.
```

## 1.5 Truthful time and status

Was: 0.2
Status: open
Depends on: 1.3 so status labels can distinguish unhealthy from not-yet-ready.

Prompt:

```text
Remove impossible timestamps and ambiguous deployment state from the control-plane-to-console path. Define how absent protobuf timestamps are represented, ensure zero/epoch values stay absent rather than becoming JavaScript Date objects, and make every deployment/build/allocation timestamp use UTC at storage and transport boundaries. A newly staged service with no build or rollout time must show an intentional label such as “Not deployed” rather than an age measured from 1970. Consolidate relative-time formatting, handle future clock skew defensively, and ensure status labels distinguish staged, queued, building, deploying, healthy, unhealthy, crashed, superseded, cancelled, and removed states once those states exist. Add codec and component tests for missing, zero, malformed, future, and valid timestamps.
```

## 1.6 Railpack as the default builder

Was: 4.1
Status: open
Depends on: none. The build executor boundary (2.4a) can wrap this later.

Most repositories have no Dockerfile. Silent Dockerfile fallback hides why a source deploy failed.

Prompt:

```text
Make Railpack a first-class field on BuildRecipe and the default for every new inspect, create, and onboarding path, while Dockerfile remains an explicit selectable recipe with dockerfile_path and context_dir. Do it as a clean cutover: builder is required, omitted or unspecified values are rejected, and all newly created repository services choose Railpack unless the user selects Dockerfile. Thread the builder choice through protobufs, JSON recipe encoding, source inspection, staged-change descriptions, console create/update/settings, claimed BuildJob, and deployment details. Dispatch in internal/builder so both paths use the same immutable snapshot workspace, exact registry capability, log reporting, cancellation, resource limits, and final digest verification. Railpack analysis failure must produce actionable detected-language and missing-start-command details rather than silently falling back to Dockerfile. Add representative Node, Python, Go, static-site, monorepo, Dockerfile, and no-buildable-source tests.
```

## 1.8 Safe deletion

Was: 0.3
Status: open
Depends on: none. Garbage collection may start as a dedicated loop and move onto the durable-work package (2.1) later.

Prompt:

```text
Replace immediate destructive deletion of projects, environments, services, domains, source archives, and volumes with explicit lifecycle semantics appropriate to each resource. User-facing delete operations must record who requested deletion, stop new work, withdraw ingress, and place recoverable resources into a tombstoned state for a configurable grace period before background garbage collection performs irreversible cleanup. Production-environment and volume deletion must require a typed confirmation tied to the current resource name and must report dependent services and domains before acceptance. Repeated requests and garbage-collector retries must be idempotent. Resource listings should hide tombstones by default but expose them to authorized recovery and operator flows. Do not claim volume recovery until the stateful volume provider provides it; until then, fail closed on deleting an attached or non-empty production volume. Cover concurrent delete/deploy, restore-during-grace, expired cleanup, and partial external-cleanup failures.
```

## 1.9 Credentials out of process arguments

Was: 0.1
Status: open
Depends on: none

Prompt:

```text
Eliminate the Cloudflare tunnel token exposure in the local product harness and establish a reusable child-process secret-handling rule. The tunnel credential must never appear in argv, inherited environment diagnostics, structured logs, test artifacts, command error strings, or process startup summaries.
```

## 1.10 Builder and agent architecture matching

Status: open
Depends on: none

An amd64 image on an arm64 node fails in a confusing way.

Prompt:

```text
Builders and agents must report their runtime architecture. Every build records the target architecture (default: the builder’s architecture). The scheduler must refuse to place an allocation on a node that cannot run that digest, with a pending message that names the required and actual architectures. Direct-image deploys resolve and persist architecture the same way. Do not silently run through qemu. Test a mismatched node, a matching node, and a reconnect that does not place the alloc on the wrong arch.
```

## 1.11 Bounded ephemeral disk

Status: open
Depends on: none. The production sandbox already exists.

Overlay writes can fill the node.

Prompt:

```text
Cap each allocation’s ephemeral writable overlay at 1 GiB by default using cgroup v2 I/O or filesystem quota on the production sandbox. When the cap is hit, the allocation must fail with a visible disk-full cause rather than filling the host. The cap is platform policy, not a customer API field, until a later quota item exists. Test that a workload writing past 1 GiB is stopped, that the host disk is not exhausted, and that the console/allocation status names disk exhaustion.
```
