# Production installations

`platformctl` owns platform installation separately from application delivery.
It places CockroachDB, control planes, consoles, Envoy, registries, builders and
agents on declared Linux hosts. Provider bindings do not determine eligibility:
a trusted, reliable home PC can run the core platform. Intermittent hosts can
run builders, stateless workloads and supplementary platform replicas.

Build the controller with `go build -o bin/platformctl ./cmd/platformctl`.
Its state, SSH keys, provider keys, database administration credentials and
recovery credentials must remain accessible without console login.

## Manifest and release

The strict YAML model lives in `internal/deploy/model.go`. Unknown fields and
multiple YAML documents are rejected. Installation and host IDs are permanent.
Set a binding to identify an imported machine. An applied binding cannot be
changed to reuse an ID for a different physical host. Instances have separate identities, so replacement
never revives a retired Envoy or agent identity.

The [examples](../infra/production/examples) cover a single host, VM plus home
PC, three reliable hosts across providers, and a fleet across regions. They
are inventory/policy templates. Replace the example IPs, provider IDs, artifact
URLs, service selections and externally managed credentials before applying.
Build the bundled operations executable and all native release artifacts with
`make package-production RELEASE_ID=r43 ARTIFACT_URL_BASE=https://your-release-service/r43 NATIVE_INPUTS=/absolute/native-inputs.json RELEASE_IMAGES=/absolute/images.json`.
Use the generated `release.json`, whose artifact hashes are computed from the actual binaries.
`images.json` pins `builder-sandbox` and `railpack-frontend` by repository and manifest digest.
The reference [toolchain Dockerfile](../infra/production/toolchain/Dockerfile) packages verified native `buildctl` and `railpack` binaries in a digest-pinned Linux base image.
Include `buildkitd` in the native inputs. Installation protects the complete OCI image closures in independent storage before starting builders. Recovery imports the sandbox into containerd and installs the frontend as a verified local OCI layout; BuildKit reads that layout without depending on the original image publisher.
They do not represent purchased or running infrastructure.

Host capacity uses CPU milliseconds, MiB of RAM and GiB of disk. `reserve` is
capacity kept outside platform components. Every component's resource budget
is added to it, and the resulting agent/builder reservations are installed with
their units. Budget a builder for its maximum admitted build, including its
sandbox and daemon. Declare capabilities, disk class, architecture, reliability,
trust, permitted roles, region and failure domain explicitly. The planner
checks observed architecture, capacity and runtime capabilities; a provider's
product name cannot substitute for them.
The agent advertises capacity after its native reservation; fleet administration
reservations subtract only additional capacity from that advertised amount. The reference hook uses zero additional SQL reservation because the full platform budget is already enforced by the agent's native cgroups. The
reservation hook coordinates admission and drains before component growth. A
builder's admission reserve covers the host's other consumers, leaving its own
declared build budget available.
During an upgrade pause, host administration reads the running agent's resource
budget and durable local identity through its native admission probe. It updates
only that enrolled agent's advertised capacity before releasing the drain.
Normal sessions, checkpoints and workload reconciliation remain paused until
production verification succeeds. Restore uses the approved fleet report and
checkpoint gates instead of this capacity admission path.

`reliableReplicas` establishes each component's reliable baseline; higher
ordinals may use intermittent hosts. All database members must be reliable. `reliable` declares that the machine remains always on; do not use it for a sleeping or periodically disconnected home PC.
Replicas of the same component use different hosts. `distinctDomains` also
requires different declared failure domains. Valid placements are preserved.
`hosts`, `capabilities`, `diskClass`, storage ownership, connectivity and the
database RTT budget further constrain placement. Insufficient placement
capacity includes a per-host explanation.

`network.peers` declares bidirectional reachability and its RTT budget input.
Reports label this latency as declared. A release's production verification
must test actual connectivity. A NAT host's SSH address is an already established
outbound reverse-tunnel endpoint; declare its gateway hosts under
`network.gateways`. Provision the outbound tunnel independently, with its own
systemd unit and external credentials. A tunnel endpoint with multiple gateways
must really fail over between them. Gateways are included in failure simulation.

Storage paths must be absolute and persistent. Local storage on the same host
is valid. `replicated` declares a shared/replicated storage service, not a claim
that copying an empty directory established replication. `storage-verify`
reports the actual verified storage hosts. Control-plane source archives on
multiple replicas need the same shared storage marker; replica state/key caches
and log journals can reside on separate disks. File logs have one writer: use
the optional shared ClickHouse backend when multiple replicas ingest logs.

`secrets` contains private absolute file references. Component `secrets` maps
relative installed file names to those references. `secretEnv` maps environment
variable names to single-line references. The controller never resolves secret
references into a plan or state document. Component reference file paths
support `{instance}` and `{host}` so the credential issuer can materialize each
replica's private files from the plan. Values are installed through SSH in private
files. External credential management supplies the shared master keyring,
component credentials and renewed wildcard certificates. Readiness must verify
that each component actually uses the provisioned credentials.
Executable staging precedes bootstrap; component credential files are installed
after key provisioning and the credential hook. Database CA/node credentials are
provisioned independently before starting CockroachDB.

A release pins its identity, configuration format, protocol, platform schema,
console schema, dependency versions, executable SHA-256 hashes and image digests.
An applied release identity is immutable. Publish a new identity to change it.
Executable artifacts are single files, including wrappers/compiled console
artifacts where necessary. Optional `tools` are pinned management executables;
`{tool.NAME}` expands to their installed versioned path. Dependencies such as
containerd, CNI, WireGuard, cgroup v2 and BuildKit must match the release and
host capability declaration. A readiness command is required for every program.

## Plan, apply and reconciliation

```sh
platformctl init-state --key-file /etc/platform/deployment.key
export PLATFORMCTL_KEY_FILE=/etc/platform/deployment.key
export PLATFORMCTL_STATE=/var/lib/platform/deployment/state.enc
platformctl plan --installation production.yaml --output plan.json
platformctl apply plan.json
platformctl reconcile --installation production.yaml
platformctl reconcile --installation production.yaml --watch --interval 30s
platformctl status
platformctl resume
```

The release defaults to `releases/<release>.yaml` beside the installation;
`--bundle` overrides it. Plans embed the exact installation and release, resource
reservations, placements, purchases and declared cost estimates, native database
changes, operation order, unmet placement requirements and availability scenarios.
A previously applied installation, bindings, release and live/retained placements
are embedded as `previous` for quiescence, drain and recovery fencing.
A missing price remains unknown. A topology shortfall is reported separately
from production durability/security validation: a valid one-host platform can
be installed even when it cannot meet the one-host-failure target.

Apply compares the state revision and observed inventory before starting. A
changed binding, host capacity/architecture/presence, replication or storage
observation makes the plan stale. Progress is persisted before side effects and
after independent verification. An interrupted plan is stored inside encrypted
state; `resume` needs no platform login or retained plaintext plan file. A second
plan cannot replace an interrupted operation. An uncertain purchase is discovered
by stable installation/host labels before further work. If discovery cannot
establish its outcome, the tool refuses another purchase.

State uses authenticated AES-256-GCM encryption with a separately retained raw
32-byte key, atomic replacement and directory syncing. An OS advisory lock covers
the controller's complete lifetime, including continuous reconciliation. All
writers must use the same state file/locking filesystem; backup copies are for
recovery, not independent active controllers. `init-state` refuses to overwrite
an existing key. Keep the encrypted state and its key in independent recovery
storage and test restoring both.

Reconciliation requires the exact applied policy and release. It can replace
stateless components, refresh credentials and apply reservations. Native backup
schedules and the independent completion timer continue separately. It preserves
offline database/agent/builder membership. It never buys
machines, changes database membership or upgrades a release automatically.
Unavailable replaced instances remain in `retained`, with their resource budgets
and identities; elapsed heartbeats never retire an Envoy member. Returning hosts
allow reconciliation to observe drain, stopped units and permanent retirement.
Explicit removal must observe the existing drain, stopped
traffic and permanent retirement protocols. `retireHosts` adds provider deletion
only after these gates and database decommission/replication verification.
Keep hosts in the inventory until their removal plan finishes.

Versioned units start at boot and restart under systemd without the controller.
Console units are independent of workload scheduling; production rejects a
scheduler-managed console. Shared SQL owns console authentication, platform
signing-key state and certificate revocations. Individual console mTLS identities
are admitted through `CONTROLPLANE_CONSOLE_CALLER_IDS`. Installed replicas do not
need a controller to remain operational. The reference operations service installs independent hourly certificate renewal.
Externally issued wildcard certificates are selected as files and checked for
identity, key agreement and remaining lifetime; their DNS-01 renewal service is external.

## Release operations contract

Hooks are release-owned commands, with separate idempotent verification commands.
Commands receive a private `PLATFORM_PLAN` JSON file and `PLATFORM_OPERATION` ID.
Lifecycle commands receive `{host}`, `{instance}`, `{address}`, `{socketHost}`, `{stateDir}`,
`{configDir}`, `{releaseDir}` and `{joins}` substitutions. Use `{socketHost}:PORT` in URLs and listen arguments: it brackets IPv6; `{address}` is the bare IP or DNS identity. Management hooks run on
a trusted reliable host; an available trusted host can replace the preferred
management host. Drain/retirement hooks run there against the target instance.
Hooks must verify native state, not merely the existence of a previous attempt's
marker. The runnable `cmd/operations` implementation supplies every hook in the
reference release. Select infrastructure, storage and endpoints in
[operations configuration](../infra/production/operations/config.example.json);
operators supply service identities and credentials. No custom lifecycle program is required.
`operationsInputs` stages those private service selections from the operator
workspace onto the chosen administration host. Native SQL/root credentials,
envelope keys, signing scopes, component certificates and runtime environment
files are generated by the reference implementation.

Lifecycle ordering is declared in [workflow.go](../internal/deploy/workflow.go).
The planner compiles that dependency graph into operations with explicit
prerequisites; plan validation rejects missing gates and incorrect actor
sequences. Fresh installation, upgrade, automatic placement and restore select
different paths through the code-owned definitions.
[Hook contracts](../internal/deploy/hook_contracts.go) declare evidence types,
native proof obligations, recovery admission and live checks before activation.
The execution engine and offline host administration consume the same contract.
Each proof must have an implemented native inspection; an unknown obligation
fails closed. Infrastructure and service configuration cannot redefine ordering
or replace inspections with assertions.

Backup completion and credential renewal use independent OS timers on selected
trusted reliable hosts (`completionHosts`, defaulting to control-plane hosts).
A redundant policy requires at least two independent completion hosts.
Database capture uses native six-hour full and ten-minute incremental SQL
schedules; each completer verifies and publishes the whole dependency graph,
then reports to the external monitor. The configured cadence is an objective,
not a measured RPO guarantee. A shared directory readback cannot establish
independent backing replicas; use a selected object service for replicated
registry/archive storage.

SQL admits the applied plan for independent backup completion and credential
renewal, including placement changes within the same authority generation.
Schedule replacement waits for durable maintenance ownership; returning old
completers cannot acquire ownership using obsolete plans. Quiescence stops old
jobs before pausing, and resume starts only the current credential timers.

Imported hosts can use a configured HTTPS Redfish computer-system endpoint for
independent power fencing. Otherwise prior units must be reachable for persistent
systemd masking, or provider power must be independently observed off. An
unreachable machine without an external fence blocks restoration.

Reachable-host fencing also inventories the actual installation's systemd units
and timers, including services absent from a saved installer snapshot. Unknown
units are stopped and persistently masked, and their definitions remain in
quarantine. A mask with a live or starting process does not establish a fence.

| Hook | Required behavior |
| --- | --- |
| `database-credentials` | Provision and verify CA, node and root/client certificates before database startup. |
| `database-init` | Initialize a fresh secure cluster once, with a distinct fresh-install admission. Restore initializes only an empty native destination and never creates platform schemas. |
| `platform-bootstrap` | Explicitly initialize the platform schema, shared keyring and all signing scopes; provision console schema/authentication. |
| `credentials` | Issue distinct component credentials, pre-enroll agents with the declared stable/intermittent host policy and failure domains, register durable ingress identities, distribute master-key versions, provision protected builder images and verify externally renewed wildcard certificates. |
| `reservations` | Drain/cordon as required and establish scheduler reservations and host policy before new platform components consume host capacity. Preserve explicit operator retirement barriers. |
| `database-verify` | Return observed `DatabaseStatus` JSON; verify native membership, range voting replicas, learners and replication convergence. |
| `storage-verify` | Return a map of storage names to `{hosts, verified}`; verify actual shared backing storage and durability. |
| `production-verify` | Check actual persistence, TLS, authorization, credentials, secret/keyring coverage and declared traffic/network paths. Topology limitations remain a separate assessment. |
| `quiesce` | Pause mutations and background work on every old replica, and independently verify the pause. |
| `recovery-protect` | Freeze and protect installation/deployment snapshots, key versions, external secrets, release bundles and all pinned release executables before activation/cutover. Verify their protected copies independently. |
| `backup-schedule` | Register installer dependencies and install/verify native full-cluster CockroachDB schedules: six-hour full, ten-minute incremental, revision history. |
| `backup` | Publish/verify a complete recovery point with `platformctl recovery`. Return Evidence JSON with `backup`, `dataLossCutoff`, `point` and exact protected `object` only after all dependencies pass. |
| `recovery-verify` | Independently verify the selected version-pinned point and exact cutoff before fencing or destructive restore; return complete Evidence JSON. |
| `recovery-finalize` | Retire the previous release's installer inventory after production verification; retained recovery points keep its artifacts. |
| `resume` | Resume mutations/background work and safely uncordon affected workers after readiness. |
| `restore` | Verify backup completeness, restore all stores and key material, and validate the requested release's exact schemas. |
| `recovery-fence` | Independently fence every previous instance and public traffic path using provider or gateway credentials before restoration. An unavailable old host must be fenced externally; heartbeat expiry is insufficient. |

The built-in `platformctl database-status --plan ... --binary ... --host ...
--certs-dir ... --databases system,platform --require-convergence` uses secure
native `cockroach node status --all` and `SHOW RANGES FROM DATABASE` commands.
Include every database that owns platform or console state. It maps native node
addresses to stable host IDs; unavailable members remain members. Voting replicas
on distinct hosts establish redundancy; learners do not. The
[CockroachDB command contract](https://docs.cockroachlabs.com/docs/v26.1/cockroach-node)
and [range output](https://docs.cockroachlabs.com/docs/v26.1/show-ranges) specify
these observations. Database program readiness must allow startup before cluster
initialization; the later native verifier establishes membership convergence.

Hetzner supports label-based discovery, explicit purchases and its server lifecycle
API. Gigahost initially imports purchased `srv_id` bindings using scoped `servers`
keys; read-only keys suffice for discovery, and lifecycle operations require write
access to the specific server. Its documented power endpoints use GET, and
cancellation uses POST. There is no assumed server-creation endpoint. See the
[Gigahost API](https://gigahost.no/en/api-dokumentasjon) and
[Hetzner API](https://docs.hetzner.cloud/). Imported Linux hosts expose discovery
and SSH installation, without purchases or provider deletion.

## Availability, cutover and recovery

A single reliable host can run the complete platform, with recovery after its
loss. Two machines cannot meet database fault tolerance. Three suitable reliable
hosts can maintain a majority after one fails, provided every other platform
dependency also survives. Larger installations can span providers and regions;
the plan reports RTT and quorum dependencies. Three desired placements are not
verified redundancy. The report requires native replication and verified storage
and evaluates database quorum (including observed range placement), control
planes, console, ingress, registry, public paths, gateways and surviving workload
capacity for each host failure. See the
[quorum requirement](https://www.cockroachlabs.com/glossary/distributed-db/quorum/).

Expanding a single-node cluster uses native join/replication procedures. Temporary
outages never reduce membership. A removal requires native decommission, zero
remaining replicas and replication verification before infrastructure deletion.

Release upgrades use a flat cutover: stage artifacts, quiesce mutations/background
work, verify a complete backup, stop the old release, start the pinned new database
processes, run one explicit SQL conversion, then start and verify the new platform
processes. There is no startup migration chain.
Production control-plane startup only validates schema 43; console schema 3 is
flat and refuses a populated unversioned or mismatched schema. Initial bootstrap
is explicit with `controlplane database bootstrap --db-url ... --keyring ...`.

The supplied `controlplane database convert --from-version 41 --backup ...
--data-loss-cutoff ... --revocations-file ... --console-schema dashboard`
preserves application/authentication data, imports old revoked certificate serials
into SQL and converts console versions 1/2 directly to flat version 3. It requires
expired old-release leases. Import the union of the old replicas' revocation files.
Schema 42 converts directly to 43 with the same complete-backup and expired-lease
gates; it already stores revocations in SQL. Other source versions require a
separately provided explicit conversion.

Rollback across a schema change uses `platformctl restore --installation ...
--bundle ... --backup ... --data-loss-cutoff ... --recovery-config ...`. Select the intended recovery
inventory and externally retained credentials. The command records and reports
the backup's cutoff before restoration. SQL desired state returns to that timestamp;
surviving newer allocations and unknown resources remain quarantined for explicit
resolution.
Independent fencing precedes restoration. Recovery on alternative hosts does not
require a lost original machine to answer SSH.
Explicit restoration can supersede an interrupted deployment recorded in the
backup; the superseded plan remains in the fencing context. An unknown purchase
must be resolved through provider discovery first.
`status` retains the last restoration's backup and cutoff independently of newer
backups; resuming an interrupted restoration reports the cutoff again.
Interrupted restoration resumes from encrypted state. A recovery inventory is
never silently substituted into an ordinary deployment.
The latest external fleet inventory is declared by `recovery.inventory` and includes
machines absent from the backup. Restore creates a host-admitted recovery generation,
starts every participant paused, reserves observed network ranges and stops for
approval of the concrete report. `resume --approve-report <digest>` continues approved
reconciliation; checkpoints, health verification, work reconciliation and a new
complete point precede resuming public mutations and automation.

The [recovery runbook](recovery.md) defines complete independent points and their
storage/verification contract. `backup` policy now declares an independent S3
target, account, primary account, failure domain, restricted writer credentials,
separate recovery-encryption key and external monitor. Recovery credentials are
distinct from production writer credentials. Recovery storage must provide
versioning, encryption and Object Lock compliance retention. Complete points
are retained for 30 days; dependency retention follows their last referencing
point. Native database schedules and an OS completion timer run independently
of reconciliation. An external monitor alerts when the latest complete database
timestamp is older than 15 minutes. Release and key activation, plus active
archive/image deletion, require protected copies first.

Initial production bootstrap requires a pre-provisioned, protected keyring and
`--recovery-config` (or `PLATFORM_RECOVERY_CONFIG`). The runbook includes command
mapping for the new hooks, configuration/policy/timer templates and the offline
release-tool contract. The two-hour recovery target includes provisioning,
transfer, restore, validation and platform availability from declaration; publish
the isolated drill report and tested fleet/capacity assumptions before claiming
that target for an installation.

`go test ./internal/deploy` exercises the acceptance topologies, stateless
replacement, controlled database expansion, incomplete replication, full-platform
failure dependencies, repeatability, stale plans, interrupted SSH, uncertain
purchases and cutover ordering. Cockroach-backed installation tests verify actual
bootstrap, shared revocations, refusal to initialize populated databases and
data-preserving conversion. Provider/SSH lifecycle tests use controlled transports;
running the templates on purchased hosts requires operator credentials and the
completed release hooks.
