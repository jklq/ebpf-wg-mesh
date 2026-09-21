# 1 — The running service is real

Until these items land, the product does not actually run a typical app, or it lies about whether it did.

Independent items in this file may proceed in parallel. 1.1 is done; see [landed.md](landed.md). 1.2 is parked; see [freeze.md](freeze.md). HTTP healthchecks remain a latched rollout gate (path, port, timeout); they are not continuous monitors. 1.4, 1.6, 1.7, 1.9, and 1.11 are done; see [landed.md](landed.md).

## 1.8 Safe deletion

Was: 0.3
Status: open
Depends on: none. Garbage collection may start as a dedicated loop and move onto the durable-work package (2.1) later.

Prompt:

```text
Replace immediate destructive deletion of projects, environments, services, domains, source archives, and volumes with explicit lifecycle semantics appropriate to each resource. User-facing delete operations must record who requested deletion, stop new work, withdraw ingress, and place recoverable resources into a tombstoned state for a configurable grace period before background garbage collection performs irreversible cleanup. Production-environment and volume deletion must require a typed confirmation tied to the current resource name and must report dependent services and domains before acceptance. Repeated requests and garbage-collector retries must be idempotent. Resource listings should hide tombstones by default but expose them to authorized recovery and operator flows. Do not claim volume recovery until the stateful volume provider provides it; until then, fail closed on deleting an attached or non-empty production volume. Cover concurrent delete/deploy, restore-during-grace, expired cleanup, and partial external-cleanup failures.
```
