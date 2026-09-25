# 1 — The running service is real

Until these items land, the product does not actually run a typical app, or it lies about whether it did.

Independent items in this file may proceed in parallel. 1.1, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9, and 1.11 are done; see [landed.md](landed.md). HTTP healthchecks remain a latched rollout gate (path, port, timeout); they are not continuous monitors.

## 1.2 Sealed secrets in the console

Was: 2.4
Status: open
Size: S
Depends on: none. The backend landed with [2.3a](landed.md#23a-secret-envelope-key-provider): sealing, masked listing, deletion, deployment pins, rollback, and desired-state decryption all exist.

A real service cannot run without credentials, and today a user cannot set one without the API. The backend is done; this is the missing half of a core feature.

Prompt:

```text
Build the console UX for sealed secrets against the existing PlatformService RPCs, following docs/frontend-handoff/2.3a.md. Public variables and sealed secrets are visibly distinct on the service variables screen. Sealing is write-only: create and update accept a value and never display it again; the list shows names, masked existence, and last-updated metadata; deletion is explicit and staged like other configuration changes. Name validation matches the backend (disjoint from public env keys, PLATFORM_ prefix reserved) and errors preserve the user's input. Deployment detail shows which sealed versions a deployment pinned without revealing values, and rollback states that it restores those pins. No console loader, action, or client log may carry plaintext beyond the single seal request. Add a Playwright journey: seal, deploy, observe the value in the running container, rotate, roll back, delete.
```
