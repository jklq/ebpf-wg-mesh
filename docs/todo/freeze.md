# Parked

Prompts here are not cancelled and not scheduled. They stay out of the [work queue](README.md#work-queue) so it only lists work that is actually next, and they keep their original IDs so dependency lines elsewhere still resolve. Record why an item was parked and what would unpark it. Move an item back into its numbered file when that condition is met.

## 1.2 Encrypted sealed secrets

Parked: reason not recorded. Unparks when secrets need to be real for a design partner, which is before anyone else's code runs on the platform.

Consequence while parked: [2.3a](landed.md#23a-secret-envelope-key-provider) landed first and owns the envelope key plus the sealed-secret backend (seal/masked-list/delete RPCs, deployment pins, desired-state decryption). What remains for this item on unpark is the console UX (write-only seal, masked existence, explicit deletion) specified in [2.3a frontend handoff](../frontend-handoff/2.3a.md). [4.2](04-developer-surface.md#42-reference-variables) still depends on this item and cannot land while it is parked.

Was: 2.4
Status: open
Depends on: none. Envelope keys come from the 2.3a in-process keyring provider; there is no external KMS to adopt later.

A real service cannot run without credentials that never appear in revision JSON, logs, or the console.

Prompt:

```text
Separate public configuration variables from sealed secrets. Persist secrets as ciphertext encrypted with per-project or per-environment data-encryption keys wrapped by the configured production key provider; never include plaintext in service revision JSON, change descriptions, audit payloads, API reads, logs, or console loader data. Only the control-plane path assembling desired state may decrypt the exact secrets needed for an assigned workload. Agents may receive runtime plaintext only for their assigned allocations, must write no plaintext desired-state JSON to disk, and must discard it when the allocation is removed. The console must support write-only create/update (seal), masked existence, and explicit deletion. Sealed values cannot be read back, un-sealed, copied into pull-request or duplicated environments, or exported through the CLI. A deployment rollback restores the variables captured with that deployment without revealing sealed plaintext. Add authorization, redaction, and no-plaintext-at-rest tests.
```
