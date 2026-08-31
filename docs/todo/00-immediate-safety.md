# Stage 0 — Immediate Safety and Truthful State

These tasks remove known credential, deletion, and status hazards before expanding the platform.

## 0.1 Remove credentials from process arguments and captured output

Prompt:

```text
Eliminate the Cloudflare tunnel token exposure in the local product harness and establish a reusable child-process secret-handling rule. The tunnel credential must never appear in argv, inherited environment diagnostics, structured logs, test artifacts, command error strings, or process startup summaries.
```

## 0.2 Make time and status rendering semantically correct

Prompt:

```text
Remove impossible timestamps and ambiguous deployment state from the control-plane-to-console path. Define how absent protobuf timestamps are represented, ensure zero/epoch values stay absent rather than becoming JavaScript Date objects, and make every deployment/build/allocation timestamp use UTC at storage and transport boundaries. A newly staged service with no build or rollout time must show an intentional label such as “Not deployed” rather than an age measured from 1970. Consolidate relative-time formatting, handle future clock skew defensively, and ensure status labels distinguish staged, queued, building, deploying, healthy, unhealthy, crashed, superseded, cancelled, and removed states once those states exist. Add codec and component tests for missing, zero, malformed, future, and valid timestamps.
```

## 0.3 Add safe deletion semantics to existing resources

Prompt:

```text
Replace immediate destructive deletion of projects, environments, services, domains, source archives, and volumes with explicit lifecycle semantics appropriate to each resource. User-facing delete operations must record who requested deletion, stop new work, withdraw ingress, and place recoverable resources into a tombstoned state for a configurable grace period before background garbage collection performs irreversible cleanup. Production-environment and volume deletion must require a typed confirmation tied to the current resource name and must report dependent services and domains before acceptance. Repeated requests and garbage-collector retries must be idempotent. Resource listings should hide tombstones by default but expose them to authorized recovery and operator flows. Do not claim volume recovery until Stage 7 provides it; until then, fail closed on deleting an attached or non-empty production volume. Cover concurrent delete/deploy, restore-during-grace, expired cleanup, and partial external-cleanup failures.
```

## 0.4 Introduce an explicit production configuration profile

Prompt:

```text
Create a fail-closed production configuration profile for controlplane, console, agent, builder, registry auth, ingress, and source storage. Production startup must reject development users, insecure cookies, wildcard or missing TLS identity, loopback-only dependencies presented as durable services, default/generated application secrets, unprotected remote Caddy administration, ephemeral source storage, disabled cgroups, and incomplete public URL or certificate configuration. Add separate liveness and readiness endpoints: liveness proves the process loop is responsive, while readiness proves required dependencies and migrations are usable without exposing credentials or private topology. Print a concise sanitized startup contract showing enabled features and dependency classes. Keep local development convenient through an explicit development profile rather than silent production fallbacks, and test every production rejection plus the valid minimal production configuration.
```
