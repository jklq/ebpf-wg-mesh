# Parked

Prompts here are not cancelled and not scheduled. They stay out of the [work queue](README.md#work-queue) so it only lists work that is actually next, and they keep their original IDs so dependency lines elsewhere still resolve. Record why an item was parked and what would unpark it. Move an item back into its numbered file when that condition is met.

## 1.2 Encrypted versioned secrets

Parked: reason not recorded. Unparks when secrets need to be real for a design partner, which is before anyone else's code runs on the platform.

Consequence while parked: [2.3a](02-host-untrusted-code.md#23a-secret-envelope-key-provider) is written to stand alone, so whichever of the two lands first owns introducing the envelope key. [4.2](04-developer-surface.md#42-reference-variables) still depends on this item and cannot land while it is parked.

Was: 2.4
Status: open
Depends on: none. May introduce envelope keys against the current file material; 2.3a later replaces the provider with KMS.

A real service cannot run without credentials that never appear in revision JSON, logs, or the console.

Prompt:

```text
Separate public configuration variables from secrets. Persist secrets as versioned ciphertext encrypted with per-project or per-environment data-encryption keys wrapped by the configured production key provider; never include plaintext in service revision JSON, change descriptions, audit payloads, API reads, logs, or console loader data. Service specs should reference secret versions, and only the control-plane path assembling desired state may decrypt the exact versions needed for an assigned workload. Agents may receive runtime plaintext only for their assigned allocations, must write no plaintext desired-state JSON to disk, and must discard it when the allocation is removed. The console must support write-only create/update, masked existence, explicit deletion, and safe copy restrictions. Rollback must restore references to historic secret versions without revealing them. Add key rotation, authorization, redaction, compromise-scope, and no-plaintext-at-rest tests.
```
