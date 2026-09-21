# Secret keyring runbook

Sealed service secrets ([2.3a](todo/landed.md#23a-secret-envelope-key-provider)) are
envelope-encrypted by a small in-process key manager. There is no external KMS:
versioned AES-256 master keys live in a provisioned keyring file that the
operator replicates to every control-plane replica, separately from the
database. CockroachDB holds ciphertext, wrapped per-environment DEKs, and
shared active/retired key-version metadata only. Plaintext master keys never
appear in the database, logs, command arguments, or errors.

## Keyring file

`CONTROLPLANE_SECRET_KEYS_KEYRING` points at the file (default
`CONTROLPLANE_STATE_DIR/secret-keys/keys.json`). Every replica reads its own
copy on every wrap/unwrap, so copying a new version to a running replica's
file takes effect without a restart.

Shape (versioned JSON, created by `controlplane keys provision` — never
hand-edit the `key` fields):

```json
{"version":1,"keys":{"kr-9f2c...":{"algorithm":"AES-256-GCM","created_at":"2026-09-21T12:00:00Z","key":"base64-32-bytes..."}}}
```

Load rules (fail closed): the file must be a regular file with no
group/other permission bits (`0600`, or `0400` once provisioned); version
must be `1`; every entry must be `AES-256-GCM` with a 256-bit base64 key;
version IDs match `[A-Za-z0-9_][A-Za-z0-9_.-]*` (1–128 chars).

Production never generates missing keys and never falls back to node-local
generated keys: a replica whose file lacks any recorded version refuses to
start and reports not-ready, and every unwrap names the missing version.

## First install (production)

1. On one machine, mint the first version (file-only, no database needed):
   `controlplane keys provision --secret-keys-keyring /etc/ebpf-wg-mesh/secret-keys/keys.json`
   prints the version, e.g. `kr-9f2c...`. `--key-id <name>` selects the ID.
2. Copy that exact file to the same path on every replica
   (`0600`, owned by the control-plane user) and store a backup copy
   separately from the database (below).
3. Activate it once against the database:
   `controlplane keys activate --db-url ... --key-id kr-9f2c...`
4. Start the replicas. Each verifies its keyring covers the active key.

Development skips all of this: the default path bootstraps its first key
automatically on startup.

## Rotation

1. `controlplane keys provision` on one machine (new version, e.g. `kr-77ab...`).
2. Copy the updated file to **every** replica and refresh the backup.
3. On each replica, confirm the version landed:
   `controlplane keys check --key-id kr-77ab...`
4. `controlplane keys activate --key-id kr-77ab...` — new wraps use it;
   the previous key retires but still unwraps.
5. `controlplane keys rewrap` until it reports zero. Each DEK row commits
   independently, so an interrupted run (crash, restart, concurrent
   activation) resumes where it stopped; re-run after any activation that
   lands mid-rewrap.
6. `controlplane keys list` shows per-key wrapped-DEK counts. When the
   retired key's count is zero, `controlplane keys delete --key-id <old ID>`
   removes its row. Deletion refuses the active key and any key with live
   wrapped DEKs.
7. Only after the row is deleted, remove the old version from the keyring
   files (keep backups until the database backup that referenced it expires).

Each version activates once; re-running `activate` for a recorded version
fails. Two concurrent activations race safely: one wins, the loser is told
to re-list.

## Verification and failure modes

- `controlplane keys list` — IDs, states, wrapped-DEK counts, and whether
  this replica's file holds each version (`LOCAL`).
- `controlplane keys check` — fails unless this replica holds every
  recorded version and every wrapped DEK probe-unwraps. Run it on each
  replica after provisioning and before activation.
- Unwrap failures classify without leaking material: `unknown-key` (no
  such registry row), `missing-provider-material` (version never
  provisioned here — copy the file), `corrupt-ciphertext` (tampered bytes,
  transplanted record, or **wrong material under a known version ID** —
  fix the file, never retry blindly), `provider-unavailable` (unreadable
  file or database error).

## Backups and restore

- Back up the keyring file **separately from the database**: encrypted
  offline media or a secrets store with different access control than the
  DB backups. Losing every copy of a version whose row still wraps DEKs
  destroys those secrets; losing the database but keeping the keyring is
  recoverable from DB backups.
- Keep every retired version's material until its registry row is deleted
  **and** the oldest database backup that could reference it has expired.
- Restore order: restore the database first, then place the keyring file
  that covers every recorded version, then run `keys check` on each
  replica before starting serving traffic.
- Ordinary restarts need no manual unlock: the file loads automatically
  under its filesystem permissions. Restrict those permissions instead of
  adding interactive ceremony.

## Retirement

Deleting the registry row (`keys delete`) is the retirement step; the
version stays usable for unwrap until then. Removing a version from the
files while its row still exists (or still wraps DEKs) fails closed on the
affected replicas — re-copy the file and re-check.
