# Platform signing keys runbook

The platform's short-lived signing keys ([2.3b](todo/02-host-untrusted-code.md#23b-platform-signing-key-lifecycle))
live in the `platform_signing_keys` table, one row per key, with private
material wrapped by the envelope key provider from
[2.3a](todo/landed.md#23a-secret-envelope-key-provider)
(`internal/controlplane/signkeys`, `internal/controlplane/secretkeys`).
CockroachDB holds the wrapped bytes and the shared active/retiring state;
the master keyring file stays provisioned to every replica separately from
the database (see [secret-keyring.md](secret-keyring.md)). Replicas hold no
node-local signing authority: they read key state through on every use,
sign only with the active key, and verify against both the active and the
retiring key.

Rotation is overlap, not ceremony: `rotate-start` demotes the active key
to retiring and activates a fresh key, both keys verify while the
longest-lived credential drains, and `rotate-finish` deletes the retiring
key once the overlap elapsed. There is no HSM, external signer, or
CA product here; every credential these keys sign is short-lived.

## Scopes and lifetime table

Each scope holds at most one active and one retiring key. The overlap
minimum exceeds the longest credential the scope signs; `rotate-finish`
refuses to delete early. Operators who raise a credential lifetime above
its default must pass a larger `--min-overlap` to `rotate-finish`.

| Scope | Key type | Credentials signed (default lifetime) | Overlap minimum |
| --- | --- | --- | --- |
| `internal-ca` | ECDSA P-256 | mTLS server certificates (720h, `ServerCertValidityHours`); mTLS client certificates for agents, builders, dashboard (24h, `ClientCertValidityHours`) | 48h (`MinOverlapInternalCA`) |
| `registry` | ECDSA P-256 | Registry pull/push capabilities (48h, `PullCredentialTTLSeconds`); exchanged registry access tokens (300s, `CredentialTTLSeconds`) | 72h (`MinOverlapRegistry`) |
| `user-assertion` | HMAC-SHA-256 | Dashboard user assertions (30s max age + 5s clock skew) | 5m (`MinOverlapUserAssertion`) |
| `dashboard-session` | HMAC-SHA-256 | Dashboard access tokens (1h) and refresh tokens (30d) | 744h / 31d (`MinOverlapDashboardSession`) |

Notes on the binding constraints:

- `internal-ca`: server leafs are the long credential, but replicas keep
  serving a retiring-chained leaf through the overlap and flip to the new
  CA on their own (per-minute refresh loop plus `RefreshServerCertificate`
  at startup), so the 24h client certificate plus renewal skew and
  reconnect margin sets the 48h floor.
- `registry`: the 48h pull capability plus a day of operational margin.
- `user-assertion`: the 30s assertion lifetime with wide clock-skew margin.
- `dashboard-session`: the 30-day refresh token plus margin. The control
  plane is the system of record for this secret; only the dashboard
  verifies with it.

## First install (production)

Development generates every scope automatically at startup (`AllowGenerate`).
Production fails closed without explicitly initialized keys: initialize
each scope once, then rotation takes over.

1. Provision and activate the envelope keyring first
   ([secret-keyring.md](secret-keyring.md)); signing keys wrap under it.
2. Start the control plane once so migrations create
   `platform_signing_keys` (schema v26+). The CLI refuses older schemas.
3. `controlplane signing-keys init --scope internal-ca --scope user-assertion --scope dashboard-session`
   (add `--scope registry` when the embedded registry is enabled).
   Each scope accepts exactly one `--hmac-secret` / `--hmac-secret-file`
   for HMAC scopes to install operator-supplied material, e.g. the
   secret the dashboard already holds; ECDSA scopes always generate.
   One secret must never seed two scopes.
4. Provision verifier-held material (below) and start serving traffic.

`controlplane signing-keys list` shows scopes, kids, states, and wrapping
keys. `controlplane signing-keys check` probe-unwraps every key on the
replica it runs against and reports missing scopes and in-progress
rotations; run it on each replica after provisioning.

## Rotation runbook

One rotation per scope at a time: `rotate-start` refuses while a retiring
key exists, and concurrent starts race safely (one wins, the loser
re-lists). `rotate-finish` takes `--min-overlap` (floored at the scope
default) and a `--force` escape hatch for tests and documented
emergencies only.

General shape for every scope:

1. `controlplane signing-keys rotate-start --scope <scope>`
2. Provision the new material to verifiers that hold it outside the
   database (per-scope steps below). Do this immediately: new
   signatures use the new key from step 1.
3. Wait out the scope's overlap (table above).
4. `controlplane signing-keys rotate-finish --scope <scope>`

### internal-ca

1. `rotate-start`. The cluster identity flips immediately; both
   generations verify.
2. Agents need no manual step: they renew client certificates against
   either CA, adopt the new cluster identity over sync, and a replica
   that starts mid-rotation accepts both generations. Each replica
   flips its own server leaf within a minute.
3. Re-issue statically provisioned client identities **before**
   `rotate-finish`, or they stop connecting when the retiring CA is
   deleted: `controlplane signing-keys issue-client-cert --caller-class
   dashboard --caller-id <id> [--ttl 24h]` (and `--caller-class builder`
   for builders), then install the emitted `ca_pem_b64` / `cert_pem_b64` /
   `key_pem_b64` bundle where each client loads it.
4. After 48h (or longer with `--min-overlap`), `rotate-finish`.

### registry

1. `rotate-start`. New capabilities and tokens sign with the new key.
2. Immediately refresh the registry's `rootcertbundle` on **every**
   replica host from the live bundle (active plus retiring
   certificates): `controlplane signing-keys export --scope registry
   --out <state-dir>/registry-auth/signing-bundle.pem`, then reload or
   restart the registry daemon so it picks up the file. Until this
   lands, the registry rejects tokens minted under the new key.
   (Restarting a replica also rewrites its file at startup.)
3. After 72h, `rotate-finish`, then refresh the bundle once more so the
   file no longer carries the deleted certificate.

### user-assertion

1. `rotate-start` (optionally with `--hmac-secret` / `--hmac-secret-file`
   when the replacement secret comes from the dashboard's host secret
   manager rather than generated).
2. Provision the new secret to the dashboard so it signs with it:
   `controlplane signing-keys export --scope user-assertion` emits the
   raw active secret to stdout (byte-for-byte, no framing) or `--out`
   (mode `0600`). The control plane keeps verifying the retiring
   secret through the overlap.
3. After 5 minutes, `rotate-finish`. The old dashboard secret is dead;
   remove it from the host secret manager.

### dashboard-session

Same flow as `user-assertion` with a 31-day overlap: `rotate-start`,
export the active secret to the dashboard's session-secret store, wait
out refresh-token drain, `rotate-finish`. The dashboard must verify
sessions against both the previous and the new secret during the
overlap; see `docs/frontend-handoff/2.3b.md`.

## Envelope interplay

- Signing-key rows record their wrapping envelope key. `keys rewrap`
  migrates them onto the active envelope key alongside DEKs
  (`RewrapAll`: idempotent, resumable, safe to re-run until zero).
- `keys delete` refuses an envelope key that still wraps signing keys
  (`WrappingCounts` feeds the same guard as DEK counts). Rewrap first,
  then delete.
- Losing every copy of a keyring version whose row still wraps signing
  keys destroys those keys; the backup discipline is the same as in
  [secret-keyring.md](secret-keyring.md).

## Verification and failure modes

- `signing-keys list` — per-scope state, kids, wrapping keys, retire times.
- `signing-keys check` — every key probe-unwraps on this replica; also
  reports scopes without an active key and rotations in progress.
- `rotate-finish` before the overlap elapsed fails with
  `ErrOverlapNotElapsed` naming the remaining time; pass a larger
  `--min-overlap` for custom credential lifetimes, never `--force`
  outside an emergency.
- A replica that cannot unwrap (missing keyring material) fails closed
  at startup and reports not-ready, naming the scope; copy the keyring
  file and re-check.
- Errors and `list` output carry identifiers and public material only.
  Private bytes cross only the `export` boundary for HMAC scopes.
