# Complete, independent recovery points

`platformctl recovery` implements the recovery contract outside the platform
scheduler and console authentication. A successful SQL backup alone never marks
a recovery point complete. The immutable point manifest names one database
timestamp and exact protected object versions for its entire dependency graph.

## What a point contains

| Dependency | Inventory and validation |
| --- | --- |
| CockroachDB | Full-cluster backup and dependent incrementals, including platform metadata, console authentication/state, identities, desired configuration, durable work, ingress state and installer recovery inventory. Every backup object is read, hashed and pinned to its S3 version. |
| Keys | Every recorded platform keyring version, console token-encryption key versions and declared external secrets. Secret bundles use authenticated encryption with a separate recovery key before upload. Historical wrapped keys and console tokens must decrypt. |
| Source | Referenced immutable source archives, including pending builds. Original object identities and SHA-256 digests survive recovery. |
| Registry | Platform-built images by `repository@sha256:digest`. Skopeo exports all architectures with digest preservation; the verifier walks every index, manifest, configuration and layer. |
| Installation | Installation manifest, immutable deployment-state snapshot, release bundles and every executable artifact and management tool for their declared architectures. |

Local certificate caches, build caches and transient observations are excluded.
ClickHouse uses its own log-retention policy. Direct-image application artifacts
remain explicit external dependencies; their registries must be available during
production recovery. Release-owned platform images are protected as complete OCI
closures, including the builder toolchain and frontend, and do not require the
original publisher during recovery.
The isolated test has no access to these external services; its release tool must
disable calls to them and report its availability checks accordingly.

The verifier opens a single `AS OF SYSTEM TIME` transaction at the selected
timestamp. It reads application references, key versions, schemas, identities,
encrypted-key/token probes and installer requirements from
`platform_recovery.public.installations`. Both releases remain registered during a
cutover; `recovery finalize` removes the previous release's inventory only after
the new platform passes production verification. Historical rows remain in backup
revision history. No recovery inventory is reconstructed from mutable image tags. Each complete
point also selects the latest independently captured installer state and service
inputs; those preserve host bindings and quarantined resources introduced after
the SQL cutoff. Points without these installer selectors are rejected.

## Independent storage

Use an S3-compatible bucket in a separate account and outside **both** the primary
site and primary storage failure domains. The configuration declares these domains
and the independent bucket's canonical owner ID, or an explicit TLS public-key
pin for a compatible service without canonical owners. Normal TLS hostname and
chain verification remains required with the pin. The adapter verifies the owner,
versioning, bucket encryption, Object Lock and resource-policy restrictions using
supported S3 operations through the native client packaged in operations and platformctl. An endpoint
must implement these operations and policy semantics; basic S3 upload support is
insufficient.

Enable object versioning, default server-side encryption and Object Lock
`COMPLIANCE` with at least 31 days of default retention. The extra day covers
native backup writes before completeness verification extends their exact versions
to the last dependent point's expiry. Complete points expire 30 days after their
database timestamp. Full backups, incrementals, artifacts, key versions and release
bundles stay protected until every dependent point expires. Retention extension
also keeps artifact receipts discoverable. There are no separate age-based deletion
rules for these categories.

Use the [bucket-policy example](../infra/production/recovery/bucket-policy.json)
with the production writer's actual principal and bucket ARN. It unconditionally
denies version deletion, governance bypass, bucket deletion and changes to
versioning, Object Lock and bucket policy. Production credentials may write, read,
list and **extend** retention; they cannot shorten compliance retention. Provision
independent operator credentials with recovery access outside the PaaS. The CLI
uses an explicit private credentials file/profile, strips inherited AWS credential
variables and disables instance credentials. Configure the same restricted writer
for CockroachDB's external connection. See
[Object Lock retention](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html).

The raw 32-byte recovery encryption key must differ from platform master keys.
Retain it and the independent credentials in an operator-accessible vault or offline
escrow that works without the platform. Include deployment-state encryption keys,
database CA/admin material, provider/SSH credentials, registry authentication,
console encryption keys and other required external secrets in the inventory.
Never put secret values or credential-bearing S3 URLs into point manifests.
A home PC can keep another copy; an intermittent machine cannot be the only
destination responsible for the 15-minute objective.

`recovery collect` runs with independent recovery administration credentials. It
traverses retained points for **all installations under the configured prefix**
and deletes only unreferenced versions whose enforced retention has expired.
Keep CockroachDB's backup prefix inside that prefix. Do not configure bucket
lifecycle deletion rules that replace this dependency traversal.

## Configuration and deployment gates

Copy [config.example.json](../infra/production/recovery/config.example.json) to an
absolute path, replace its example identities, file paths and digests, and set
mode `0600`. The recovery key and credentials are also private absolute files.
Set `PLATFORM_RECOVERY_CONFIG` or pass `--config` on every command. The example is
an inventory template, not a configured account or a runnable release.

`files` declares immutable identities and paths for installer-owned requirements.
Use a new secret identity for changed secret material. Keyring identities are
their actual version names. Nonsecret digests can be computed from local files
during protection and registration. Freeze the installation and deployment-state
files for the deployment attempt; do not point at the continuously rewritten
controller state file. The deployment snapshot must contain its plan, bindings,
placements, retained instances and resumable progress. Preserve its decryption key
as an encrypted external-secret dependency. Declare every external secret and
console key version required by that installation; the verifier cannot discover
undeclared external services from a SQL backup. A protected release bundle
automatically adds all its program and tool artifacts, so these executables do not
depend on a hand-maintained second list.
Supply self-contained executables or declare every supporting runtime/library
bundle as additional protected files. The offline release tool must unpack those
bundles; an executable that depends on missing adjacent files cannot pass a drill.

Before a first installation, explicitly provision the initial master key:

```sh
controlplane keys provision --secret-keys-keyring /etc/platform/keyring.json --key-id k1
platformctl recovery protect-files --config /etc/platform/recovery.json
```

Production database bootstrap and `controlplane keys activate` require a matching
protected copy through `--recovery-config` (or `PLATFORM_RECOVERY_CONFIG`). Generate
and protect a new key version before activation; do not overwrite an existing
version's material. Initial bootstrap cannot invent unprotected key material.

The release operations executable supplies these hooks using the CLI:

| Hook | Command and verification |
| --- | --- |
| `recovery-protect` | Freeze installer snapshots, `recovery protect-files`, then `recovery verify-files`; runs before activation or release cutover. |
| `backup-schedule` | `recovery schedule`; verifies protected installer files, registers their requirements and installs/verifies the native schedules after bootstrap/conversion. |
| `backup` | `recovery complete`; return its full Evidence JSON, including `point` and `object`. Verification must re-read the selected protected point with `recovery verify --backup ... --check-content`. |
| `recovery-verify` | `recovery verify --backup "$PLATFORM_BACKUP" --check-content`; return Evidence for the exact requested cutoff before fencing or restore. |
| `recovery-finalize` | `recovery finalize` after production verification. Its verification checks the registered release inventory. |

The controller rejects bare `{backup, dataLossCutoff}` assertions, missing
dependencies, invalid backup chains, expired protection and mismatched timestamps.
It rechecks critical recovery gates when resuming an interrupted operation.
Upgrades require a complete point no older than 15 minutes before stopping the old
release. First installation finishes with a complete point. Ordinary reconciliation
does not drive periodic backups.

Production control planes use `--recovery-config` to protect archives and images
before active garbage collection deletes them. Missing configuration or failed
export/protection blocks deletion. This does not give active storage collectors
permission to delete protected object versions.

## Native cadence and external monitoring

Create the CockroachDB external connection independently using the restricted
backup writer, the independent bucket and its native backup prefix. The recovery
configuration stores only its `external://name` identifier. Then run:

```sh
platformctl recovery schedule --config /etc/platform/recovery.json
```

Schedule installation and completion inspect the supported external-connection
definition in memory and reject a native bucket, prefix or endpoint that differs
from the independently inventoried store. Credentials are never copied into the
point manifest or diagnostics.
Use a dedicated collection for these schedules. Any administrative backup added
to it must also use `revision_history`; do not mix snapshot-only manual backups
into the scheduled chains.

The full-cluster schedule uses `revision_history`, full backups at
`0 */6 * * *`, and incremental backups at `*/10 * * * *`. This normally produces
35 incrementals between six-hour full backups, below the documented maximum of
48. Schedule verification rejects changed cadence/destination, missing revision
history and paused schedules. Chains with gaps or more than 48 incrementals cannot
become complete. CockroachDB schedules run independently of the PaaS scheduler.
Provision the required CockroachDB backup capabilities/license for the pinned
release. See [native schedules](https://docs.cockroachlabs.com/docs/v26.1/create-schedule-for-backup)
and [full/incremental guidance](https://docs.cockroachlabs.com/docs/stable/take-full-and-incremental-backups).

Run completion from an OS timer on a reliable independent management host every
minute, after native backups finish. The supplied
[service](../infra/production/recovery/platform-recovery-complete.service) and
[timer](../infra/production/recovery/platform-recovery-complete.timer) are templates.
Configure the active source backend if archives may need copying:

```sh
platformctl recovery complete --config /etc/platform/recovery.json \
  --source-config /etc/platform/source-backend.json
```

Completion selects the latest full chain and its latest completed endpoint by
default. `--subdirectory` and `--timestamp` select another covered timestamp. It
checks native backup files, inventories exact S3 versions, rechecks the chain after
the walk, resolves dependencies, verifies their content and decryption, extends
retention and only then publishes the protected point. A racing backup completion
causes a retry rather than publication. Allow enough SQL revision-history/GC
retention to read the metadata at the selected timestamp.

Run `recovery monitor` from outside the primary site/account at least every minute.
It needs only independent storage, the recovery key and the installation identity;
it does not open the primary database or require active artifacts. Forward a
nonzero exit or process absence to the external alert service declared by the
installation. Freshness is the age of the latest **complete database timestamp**,
reported as `ageSeconds`; verification completion time cannot advance it. An age
over 900 seconds fails. JSON includes missing dependencies, failed completion
attempts and retention failures. If the primary cannot publish a failure report,
the independent age alert still fires. Monitor job execution as well as its output.

Ten-minute incrementals leave five minutes for backup execution and completeness
work. Large first exports or repeated failures can exceed that allowance; protect
artifacts before they become hot dependencies and measure transfer/verification
costs on the intended fleet.

## Isolated restoration and production recovery

Save the exact `object` JSON returned by completion, or retain its version-pinned
`s3://bucket/key?versionId=...` URL. `recovery verify` and `recovery drill` accept
exactly one of `--point` and `--backup`. Neither resolves a mutable tag or an
unversioned catalog name.

```sh
platformctl recovery verify --config /etc/platform/recovery.json \
  --backup "$RECOVERY_POINT_URL" --check-content
sudo platformctl recovery drill --config /etc/platform/recovery.json \
  --backup "$RECOVERY_POINT_URL" --declared-at "$RECOVERY_DECLARATION_TIME"
```

Record the declaration timestamp **before provisioning starts**. The drill copies
and hashes protected data into a fresh private workspace, decrypts secret bundles
there and executes the selected point's pinned release recovery executable in
fresh Linux network/PID namespaces. Only loopback exists, production endpoints are
unreachable, and storage/provider credentials are removed from its environment.
Processes die when the namespace runner exits. The recovery tool must use only
the fresh workspace, never production storage mounts. Namespace creation requires
appropriate privileges. The internal `recovery-isolated` entry point refuses the
caller's network namespace before executing any release tool.

Provide enough temporary disk for the backup objects, artifacts and extracted OCI
layouts. Set `TMPDIR` to the intended recovery scratch disk and include its capacity
in the published assumptions.

`drillCommand[0]` is replaced by the protected executable with
`drillCommandDigest`; its remaining arguments are followed by `WORKSPACE INPUT_JSON`.
The private input contains the point, sanitized configuration and a `files` map
from `kind/id` to materialized dependency paths. Native backup objects retain their
keys under `WORKSPACE/database/<object-key>`. The release tool must:

1. Provision the declared test fleet and dependencies using the protected release
   tools. The workspace has no network access to package repositories.
2. Start a fresh CockroachDB cluster with local external I/O rooted at
   `WORKSPACE/database`, then use supported full-cluster `RESTORE FROM <subdirectory>
   IN 'nodelocal://1/<backupPrefix>' AS OF SYSTEM TIME <point timestamp>`.
3. Pause any restored native backup schedules before starting platform processes.
   Install the protected keyring versions, console keys, source archives and
   external secrets. Disable outbound integrations and production listeners.
4. Start a local registry with loopback TLS, a local auth file and a
   `registryCertDir` under the workspace containing Skopeo's CA/client certificates. The
   validator imports each OCI export with Skopeo and checks retrieval by the
   original digest. Include Skopeo as an explicit protected `tool` file in config.
5. Start the restored platform with isolated addresses and print one JSON object:

```json
{
  "databaseURL": "postgresql://root@127.0.0.1:26257/platform?sslmode=disable",
  "keyringFile": "/workspace/keyring.json",
  "consoleKeyFiles": ["/workspace/console-token.key"],
  "registry": "127.0.0.1:5443",
  "authFile": "/workspace/registry-auth.json",
  "registryCertDir": "/workspace/registry-ca",
  "sourceDirectory": "/workspace/source-archives",
  "availabilityURL": "http://127.0.0.1:8080/ready"
}
```

Use the actual fresh workspace paths. Keep the processes running after the tool
returns; namespace cleanup stops them. SQL integrity checks, exact schemas,
platform identities, local key coverage, wrapped DEKs/signing keys, console token
decryption, source availability at original keys, complete image inventories and
HTTP availability must all pass.
Missing keys or images prevent a successful drill. The release operations/offline
provisioning executable remains operator supplied, as in production deployment;
the example release does not supply an infrastructure-specific implementation.

## Restore against a surviving fleet

Obtain the protected installation, encrypted deployment state, keys, exact release
bundle and tools using independent operator access. Select an explicit point and
its cutoff. The SSH driver verifies the complete point and its corresponding
release before provisioning, then streams protected executables to the target
with digest checks. Recovery works without the original download service or an
SSH response from lost infrastructure. The target may use either provider or
imported hosts, with different counts and placements.

Restore into a fresh CockroachDB cluster. `recovery-database` initializes native
membership and verifies an empty destination; it must not bootstrap platform or
console schemas. `restore` completes full-cluster restoration and pauses restored
native backup schedules before any platform process starts. An occupied destination
is rejected, never cleared automatically. See the
[CockroachDB restore requirements](https://docs.cockroachlabs.com/docs/stable/restore).

The encrypted deployment state records the operation outside restored SQL before
any side effects. Repeating the same restore or running `resume` continues the
same generation. `status` reports the cutoff, pinned release, fencing, phase,
unresolved resources and elapsed time; a later backup does not replace the recorded
restore cutoff. Keep this state and its decryption key on the independent operator
host throughout recovery.

```sh
platformctl restore --installation /etc/platform/restore-target.yaml \
  --bundle /etc/platform/selected-release.yaml \
  --state /var/lib/platformctl/installation.state \
  --key-file /etc/platform/deployment-state.key \
  --backup "$RECOVERY_POINT_URL" --data-loss-cutoff "$RECOVERY_CUTOFF" \
  --recovery-config /etc/platform/recovery.json
platformctl status --state /var/lib/platformctl/installation.state \
  --key-file /etc/platform/deployment-state.key
```

### Authority and pause

Installation identity stays stable. Recovery creates a new generation; normal
replica failover advances an authority epoch within that generation. The revision
cursor orders changes within one authority. Agents validate installation and
generation before observing epochs or cursors, so a high old-generation epoch
cannot fence out restored commands. RPCs and endpoint discovery cannot admit a
generation. Only a private local admission file supplied through host administration
does so. Old generations are recorded as retired locally.

The credentials hook provisions admission for a normal first installation and
calls `platformctl recovery initialize-authority`. The recovery authority hook
uses `reset-authority`, replaces all internal signing scopes without retiring-key
overlap, removes restored client certificates/bootstrap tokens, and provisions new
client identities and trust on every admitted participant. Console JWTs bind to
installation and generation, invalidating old sessions even if a signing key is
still cached. Registry authority is also replaced. Recover the historical envelope
keys needed to decrypt data; replacing signing authority does not discard them.

Admission files use mode `0600` and an absolute path:

```json
{
  "installationId": "production",
  "generation": "persisted-recovery-generation",
  "clusterId": "sha256-of-the-current-internal-CA-bundle",
  "paused": true,
  "checkpoints": false
}
```

The installer sets `CONTROLPLANE_AUTHORITY_FILE`, `AGENT_AUTHORITY_FILE`,
`BUILDER_AUTHORITY_FILE` and `DASHBOARD_AUTHORITY_FILE` to each instance's
`authority.json`. The release tool supplies the files before starting units.
Agent TLS caches also carry the generation marker; provision it with new host-admin
client material. A missing/mismatched marker forces replacement of the cached identity.
`controlplane signing-keys issue-client-cert --caller-class agent --caller-id ...`
issues and records an agent identity over the host administration channel while
normal enrollment RPCs remain paused. Install its private certificate/key/CA output
and generation marker on the admitted agent. Fresh agents are enrolled into restored
desired host state by approved reconciliation before checkpoint delivery.
The shared SQL authority row prevents a stale unpaused console/control-plane file
from overriding the restored pause. All replicas start with mutations, webhooks,
builds, placement, failover, deletion, artifact cleanup and certificate issuance
paused. Agents still discover durable inventory, and authenticated operator reads
remain available. Paused builders do not recover executors, claim work or collect
workspaces. Paused agents do not run periodic reconciliation or image cleanup.

Fence the previous installation using provider or host controls and replace its
credentials before restoring. Fence its public traffic paths and access to shared
database, storage, registry and external integrations. DNS changes, heartbeat
expiry and restored lease counters are insufficient. An unreachable host must
remain externally isolated; inability to contact it cannot prove a fence.

### Inventory, reservations and approval

`installation.recovery.inventory` names the latest external `FleetInput` JSON
on the administration host. Maintain this inventory independently of backups and
SQL, including machines added after the cutoff. The release inventory hook merges
provider/host records with authenticated agent reports from
`StateDir/recovery-inventory/<generation>/`. Reachable agents report nonsecret
allocation identities, actual revisions, rollout generations, addresses and durable
volumes. Unreachable hosts carry their last recorded protected ranges and verified
isolation/decommission status. A fresh capture timestamp is required after recovery
starts. A list obtained only from the restored database cannot locate the entire
surviving fleet.

The `fleet-inventory` helper replaces caller-supplied desired state with a consistent
snapshot of restored SQL. The report identifies post-cutoff releases/resources,
missing hosts, unresolved authority, changed allocations, network conflicts,
allocations eligible for adoption and changes needed to return to restored desired
state. Unknown machines, workloads and volumes remain quarantined. Absence from an
older backup never authorizes deletion. Store identity conflicts and allocations
observed on the wrong host block admission.

Before approval, `reserve-fleet` protects the union of desired and observed network
identities, subnets and individual addresses. Allocators skip these reservations;
the numeric identity counter advances past observed identities. Reservation writes
are additive. Neither an inventory refresh nor an unreachable host releases a
range; inventory resolution or verified decommission must do so explicitly.

The controller stops at its internal `recovery-approve` gate after reservations
are verified. Inspect the concrete report from `status`, resolve blocking conflicts,
and approve its exact `reportDigest`:

```sh
platformctl resume --state /var/lib/platformctl/installation.state \
  --key-file /etc/platform/deployment-state.key \
  --recovery-config /etc/platform/recovery.json --approve-report "$REPORT_DIGEST"
```

If the external inventory changes before reconciliation starts, use
`resume --refresh-inventory` with the same state, key and recovery configuration.
This invalidates report approval, reruns inventory/reservations and retains the
operation's generation. Once approved reconciliation has started, resume that
operation before requesting a new inventory. Elapsed-time display updates do not
alter the approval digest.

### Release operations and return to operation

Each recovery hook returns typed `Evidence.recovery` for the installation and
generation, with independently verified `checks`. The pinned operations executable
owns infrastructure effects and observation, as in normal production deployment.
Hooks must be idempotent across interruption and verify actual outcomes. Helpers
are host-admin commands using a private `--database-url-file`, independent of
platform RPCs and console login.

| Hook | Required effects and verification |
| --- | --- |
| `recovery-fence` | Provider/host fence and credential replacement, including unreachable old infrastructure. |
| `recovery-database` | Native membership ready; `recovery empty-destination` proves no user schema initialized. |
| `restore` | Native full-cluster restore completes at the selected timestamp with the selected release; pause restored native schedules. |
| `recovery-authority` | `recovery reset-authority --keyring ...`, new CA/client identities, invalid console sessions, registry authority, host-admin admission and all participants paused. |
| `recovery-inventory` | `recovery fleet-inventory --fleet ...`; merge the latest external inventory and actual host reports. |
| `recovery-reserve` | `recovery reserve-fleet --report ...`; return the reserved report digest. |
| `recovery-reconcile` | Apply only approved restored desired state and preserve quarantined resources; return the approved report digest. |
| `recovery-checkpoints` | Enable `checkpoints: true` through host administration after approval. Apply complete checkpoints to every admitted agent and publish restored ingress state for acknowledgment verification. Record generation, epoch, cursor and completeness acknowledgments. Incremental updates and normal automation remain paused. |
| `recovery-work` | Invalidate stale build ownership, establish new worker leases and reconcile external effects before retrying non-idempotent work. Use the durable work/journal interfaces and refresh projections after administrative changes. |
| `production-verify` | Database health, key access, overlay connectivity, image access, ingress acknowledgments, certificate trust and console login all pass. |
| `backup-schedule`, `backup` | Re-establish independent native schedules and verify a new complete point whose cutoff is after recovery started. |
| `recovery-resume` | `recovery resume-authority` validates the independently recorded plan/progress, report approval, checkpoint acknowledgments and new point before clearing shared SQL pause. Update local admission, restart all participants and verify all resumed. |

The controller passes `PLATFORM_INSTALLATION`, `PLATFORM_RECOVERY_GENERATION`,
`PLATFORM_PLAN`, `PLATFORM_RECOVERY_OPERATION`, `PLATFORM_BACKUP` and
`PLATFORM_DATA_LOSS_CUTOFF` to hooks. The operation file includes the report,
reservations/approval digests, acknowledgments and verified new point. Hook
verification must return these receipts from observed effects, not merely echo
requested booleans. `reset-authority` proves only signing-key/registry replacement;
the release tool must complete identity distribution and prove the remaining checks.
`resume-authority` proves the shared authority transition; the tool must finish the
local restart and all-participant verification.

The restore plan contains no ordinary bootstrap, drain, retirement or infrastructure
deletion. Public mutations and background automation resume only after successful
verification, checkpoints, work reconciliation and a new complete backup. Unknown
resources still require explicit resolution after recovery; normal cleanup never
uses their absence from the backup as permission to remove them.

## Tested scope and the two-hour target

A drill report publishes declaration and availability timestamps, elapsed seconds,
tested fleet size, transferred data bytes, measured transfer rate and declared
capacity assumptions. It succeeds within the recovery target only when platform
availability occurs within 7,200 seconds of declaration. Publish that report with
the host CPU/RAM/disk, available restore capacity, database/artifact sizes and
storage/link throughput used for the test. Fleet size and capacity assumptions
must describe the fleet actually provisioned by the release tool.

Repository tests exercise native CockroachDB 26.1.0 full/incremental backups and
timestamped restore into a second local cluster, plus restoration of the real
platform schema with signing identities, integrity checks and console decryption.
Unit tests cover missing dependencies/blobs, wrong keys, immutable secret
versions, stale points, retention failures, cross-installation collection and
deletion gates. A privileged Linux namespace test verifies loopback and blocked
external connectivity. These tests establish correctness of those mechanisms;
they do not establish a production fleet's two-hour recovery time or an S3
provider's Object Lock implementation. Run an isolated drill and storage acceptance
test with the real release, endpoint and representative data before publishing
that fleet's measured objectives.

Surviving-fleet tests cover a lost control plane with agents at newer epochs/cursors,
post-cutoff allocations, an unknown host and an isolated unreachable host. They
verify lower-position complete checkpoints, preserved unknown resources, accurate
adoption/quarantine reports, protected ranges and retired authority rejection.
The deployment drill interrupts restore on an alternative imported host and
resumes the same generation through exact report approval. Native tests reject an
occupied restore destination and verify old certificates fail against the replaced
CA. Two live TLS replicas verify a shared pause overrides stale local files while
authenticated inspection stays available. Fresh-agent checkpoint admission is also
exercised.

Run complete loss and fresh-host drills with the actual pinned release and external
inventory, including an unreachable host and an interrupted restore. Repeat them
after schema, key-management, backup-format or recovery-protocol changes. Repository
drills do not replace provider fencing and infrastructure acceptance tests.

```sh
go test ./...
go test -tags=integration ./internal/recovery -count=1
go test -tags=integration ./internal/controlplane -count=1 \
  -run 'TestSharedRecoveryPause|TestExplicitInstallation|TestFlatInstallation|TestConversionRequires|TestAgent.*Sync'
go test -c -o /tmp/platform-recovery-tests ./internal/recovery
sudo /tmp/platform-recovery-tests -test.run TestFreshNetworkNamespace -test.v
```
