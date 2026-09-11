# OVH stress fixture

`cmd/testvm` can provision disposable OVH VMs and run a seeded stress campaign
against the real control plane, CockroachDB, containerd, agents, WireGuard, and
eBPF runtime. It reuses the Hetzner harness's Linux builds, cloud-init templates,
deployment scripts, mTLS client, workload helpers, and host diagnostics.

This fixture searches for breaking points. A nonzero exit can mean a product
invariant failed, the selected machines ran out of capacity, a latency/error
threshold was exceeded, or infrastructure/setup failed. Read the recorded error
before classifying it as a product bug.

## Credentials and permissions

Put credentials in the ignored `.env.ovh` file with mode `0600`, or export them.
Process environment wins over dotenv files; `.env` is loaded before `.env.ovh`.

```dotenv
OVH_ENDPOINT=ovh-ca
OVH_APPLICATION_KEY=...
OVH_APPLICATION_SECRET=...
OVH_CONSUMER_KEY=...
OVH_CLOUD_PROJECT=...
```

The API endpoint belongs to the account/credentials, independently of the VM
region. Supported endpoints are `ovh-eu`, `ovh-ca`, and `ovh-us`. A Canadian
account can expose European VM regions. Use `-ovh-endpoint` to override the env.
The project can be omitted only if the key can list projects and exactly one
project is accessible.

Grant these permissions when creating the consumer key:

| Method | Path | Use |
| --- | --- | --- |
| GET | `/cloud/project` | Project discovery |
| GET | `/cloud/project/*` | Read flavors, images, instances, and SSH keys |
| POST | `/cloud/project/*/sshkey` | Create an ephemeral SSH key |
| POST | `/cloud/project/*/instance` | Create hourly test instances |
| DELETE | `/cloud/project/*/instance/*` | Destroy test instances |
| DELETE | `/cloud/project/*/sshkey/*` | Delete the ephemeral SSH key |

Alternatively, allow GET, POST, and DELETE on `/cloud/project/*`, plus
GET `/cloud/project`. A project-specific prefix can replace the project wildcard.
Fault injection uses SSH, not additional OVH API mutations. The OVH SDK also
uses the public `/auth/time` endpoint for request signing. No PUT permission is
needed. An empty path in a credential rule does not grant project access.

## Inspect and run

Requirements: Go, OpenSSH (`ssh`, `scp`, `ssh-keygen`), outbound HTTPS, and network
access to the VMs' SSH and gRPC ports. OVH mode uses the official Go SDK directly;
it does not require OpenTofu or OpenStack credentials. Use an Ubuntu 24.04 x86
image and an available Linux flavor with sufficient quota. Cloud-init installs
the host packages, including Python 3 for the HTTP load generator.

Cross-agent workload traffic rides WireGuard. The agent's IPv6 `advertise_addr`
remains its mesh host identity, while its independently advertised WireGuard
endpoint selects the reachable UDP underlay. OVH runs use each instance's public
IPv4 endpoint because the observed per-instance public IPv6 `/64` assignments in
`GRA11` and `SBG5` had no inter-instance connectivity. This does not change the
dual-stack overlay or peer AllowedIPs. Before any load runs, the fixture waits
for every configured peer to complete a WireGuard handshake and reports failure
as infrastructure/setup rather than a product breaking point.

Inspect the seeded workload without credentials, builds, or cloud access:

```sh
make plan-stress-ovh ARGS='-seed 42 -agents 2'
```

Inspect the region's live flavor/image catalog without creating anything:

```sh
go run ./cmd/testvm -provider ovh -ovh-action catalog -ovh-region GRA11
```

Verify the chosen flavor's **hourly price including applicable tax** in your
OVH account. Set `OVH_VM_HOURLY_RATE` in your shell to that number. This variable
is only a convenience for the following commands; the harness requires the
explicit `-ovh-hourly-rate` flag. Prices and budget must use the same currency.
The harness does not assume that account credits are USD or convert currencies.

```sh
# Read-only infrastructure plan and budget check:
go run ./cmd/testvm -provider ovh -ovh-action plan \
  -ovh-region GRA11 -ovh-flavor b3-8 \
  -ovh-hourly-rate "$OVH_VM_HOURLY_RATE" -budget 5

# Small first run: two agents, two stages, two new services per stage.
make test-stress-ovh ARGS="-seed 42 -stress-stages 2 -services-per-stage 2 \
  -stage-duration 10s -timeout 30m \
  -ovh-hourly-rate $OVH_VM_HOURLY_RATE -budget 5"

# Cover the seven core fault types if no earlier breaking point stops the run:
make test-stress-ovh ARGS="-seed 42 -stress-stages 8 -services-per-stage 1 \
  -ovh-hourly-rate $OVH_VM_HOURLY_RATE -budget 5"

# Cover the extended faults too, and at least one compounded stage:
make test-stress-ovh ARGS="-seed 42 -stress-stages 12 -services-per-stage 1 \
  -ovh-hourly-rate $OVH_VM_HOURLY_RATE -budget 5"

# Increase both fleet size and the workload:
make test-stress-ovh ARGS="-seed 43 -agents 4 -services-per-stage 8 \
  -max-concurrency 128 -http-targets 8 -timeout 1h \
  -ovh-hourly-rate $OVH_VM_HOURLY_RATE -budget 5"
```

Override `-ovh-region`, `-ovh-flavor`, and `-ovh-image` using catalog results.
`GRA11`, `b3-8`, and `Ubuntu 24.04` are selection defaults, not an availability
or price guarantee. All VMs use the selected flavor. Each run creates one VM
hosting two control-plane processes and a single-node CockroachDB, plus 2–32
agent VMs. The topology inherits the existing harness's colocated control planes
and database; this does not test failure of independent database replicas.

`-provider ovh -scenario service-rollout -ovh-action run` also runs the existing
VM smoke scenario on OVH. `-scenario stress` works with the existing Hetzner
provider and its fixed two-agent topology as well.

## Workload and invariants

The default campaign has eight stages. Each stage adds four services; RPC and
HTTP concurrency starts at one, doubles each stage, and caps at 128. Half the
services are single-stack, half dual-stack. Resources use the platform minimum
of 250 millicores and 256 MiB, so an explicit scheduler capacity limit may be
the first breaking point on small machines. Reduce `-services-per-stage` to
reach later fault scenarios before exhausting capacity, or increase
`-agents`/the flavor for a larger run.

Each stage performs:

1. Create services across `-tenants` projects and release their environments.
2. Verify acknowledged service identity, revision, and content through both
   replicas with non-redirecting clients (a standby redirect is not treated as
   agreement with the owner), exactly one healthy serving allocation per service (a
   draining predecessor is tolerated, a second serving allocation is not),
   actual workload HTTP responses, and denial of cross-project traffic with both
   a source-side success control and a target-side reachability control.
3. Run concurrent GetService, GetServiceStatus, ListServices, and blocking-watch
   RPCs. Every watch response is checked for index regression and foreign
   services. At the same time, run up to `-http-targets` independent HTTP flows
   inside workload network namespaces, each targeting a distinct service and
   preferring a same-project destination on another agent; the histograms are
   merged before percentiles. Fail on wrong HTTP content, excessive error rate,
   or excessive p99.
4. After the first stage, delete and recreate a seeded service, check the old ID
   disappears from both readers, and verify convergence again.
5. Race conflicting seeded writes to one service from both replicas and require
   the committed value to be one of the attempted values (this also runs once
   during the fault window, on a service excluded from the sequential updates).
6. Apply the stage's faults while read/watch load, workload HTTP, and seeded
   service updates run. Heal, wait for each fault's readiness condition, release
   pending changes, and require recovery of all services. Faults that do not
   target an agent must leave the workload data plane within threshold; agent
   faults may disrupt it but it must recover.
7. Require exactly one live singleton lease, a non-decreasing fencing token, and
   exactly one replica serving owner-local RPCs (both serving is split brain).
   Then write one service through one replica and require a blocking watch on the
   other replica, using a raw client that does not follow live-owner redirects,
   to observe the acknowledged revision.
8. Require each agent's containers, network namespaces, and persisted desired
   state to exactly match the converged allocation set, so teardown leaks cannot
   hide.

The first stage is fault-free. The seven core faults are scheduled first, so a
default eight-stage campaign covers all of them; the extended faults follow and
compounded stages appear once the campaign is long enough:

| Fault | Mechanism | Domain |
| --- | --- | --- |
| Control-plane crash | SIGKILL the current owner; systemd restarts it | core |
| Control-plane pause | SIGSTOP/SIGCONT the current owner | core |
| Agent crash | SIGKILL a seeded agent; systemd restarts it | core |
| Agent partition | Drop TCP both ways to both control-plane ports | core |
| Database pause | SIGSTOP/SIGCONT CockroachDB | core |
| CPU pressure | Restrict the current owner to 10% of one CPU | core |
| Memory pressure | Cap the current owner at `MemoryHigh=48M`, `MemoryMax=96M` | core |
| Database restart | Restart CockroachDB and wait for it to accept queries | extended |
| Agent WireGuard partition | Drop the agent's UDP port 51820 both ways | extended |
| Disk I/O throttle | Cap CockroachDB write bandwidth via a cgroup | extended |
| Clock skew | Step an agent clock forward 5s, then reset it | extended |

Every third fault stage compounds a second fault from a different failure domain
(never two faults on the colocated control-plane host), so multi-fault recovery
is exercised without manual flags.

The workload generator follows explicit owner redirects only to the fixture's
known replicas. It does not blindly retry mutations after transport failures.
An unacknowledged update may have committed: the oracle permits its prior or any
attempted value until both durable readers agree. An acknowledged update must
survive with its returned revision and marker. Other values fail the invariant.
Transport-level failures (`Unavailable`, `DeadlineExceeded`, `Canceled`,
`Unknown`, `ResourceExhausted`, `Internal`) may have committed and are treated
as ambiguous; `Aborted` is an optimistic-concurrency loss and is treated as
definitively not committed. The count and status codes are retained in the
event log.

The default healthy-window limits are 1% RPC/HTTP errors and 2s p99. Override
`-max-error-rate`, `-max-p99`, and `-rpc-timeout` as appropriate. Blocking-watch
latencies include their intentional one-second wait. Fault-window RPC errors
are measured but are not subject to the healthy-window threshold; restoration
of the service invariants is required after healing. `-recovery-timeout` bounds
each convergence/release phase. The overall `-timeout` also includes builds,
provisioning, deployment, and all stages.

## Artifacts and replay

Artifacts default to `artifacts/e2e-vm/<run-id>/`:

- `ovh-plan.json`: endpoint, region, selected flavor/image, VM count, budget.
- `ovh-resources.json`: atomic cleanup manifest, including partial provisioning.
- `stress-plan.json`: seeded faults (including compounded stages), targets, load
  ramp, and service update order.
- `stress-events.jsonl`: timed creates, updates, deletes, contention attempts,
  faults, HTTP flow plans, watch probes, load windows, and invariant failures.
  RPC codes, acknowledged revisions, and watch anomalies are retained.
- `stress-summary.json`: request counts, RPS, p50/p95/p99, error counts, watch
  anomalies, the fencing token per stage, recovery times, the last passed stage,
  and the first failed stage and reason.
- `stress-stage-*.txt`: host pressure, memory, CPU, restart, socket, firewall,
  disk, and kernel diagnostics at each stage boundary.
- `bin/` and `hosts/`: the built binaries and shared harness host diagnostics.

On failure the command exits nonzero after attempting diagnostics and cleanup.
Setup failures before the stress scenario starts do not produce a stress summary;
use command output and the infrastructure/host artifacts in that case.

Replay with the same seed, workload flags, source revision, and infrastructure
selection, but a **fresh run ID**. The default ID is based on the current time.
The binaries in the artifact directory preserve what was deployed. To minimize
a failure, rerun with the same seed and fewer stages first; changing service or
agent counts changes the generated scenario.

The schedule is deterministic; VM execution and timing are not. This borrows
seeded exploration, explicit safety checks, and a fault/heal/recovery cycle from
[TigerBeetle's simulation testing approach](https://tigerbeetle.com/blog/2023-07-06-simulation-testing-for-liveness/).
It is a real-system stress fixture, not a virtual-clock simulator or a claim of
bit-for-bit replay. HTTP is a closed-loop Python generator on an agent, and RPC
load originates on the runner; generator CPU, SSH probe overhead, and runner
network latency can limit the measured rate. Clock skew is bounded to 5s to stay
inside TLS/JWT validity windows. Both control planes and CockroachDB are
colocated on one host, so control-plane-to-control-plane and control-plane-to-
database network partitions cannot be tested. There is no on-disk corruption,
multi-node database failure, eBPF program unload, or exhaustive interleaving
search in this fixture.

## Cost and cleanup

The preflight rejects estimates above `-budget` (default 5), using:

```text
(agent VMs + 1) × supplied hourly price × (ceil(timeout in hours) + 1)
```

The extra hour reserves compute allowance for cleanup and billing rounding.
The command requires a positive supplied rate, limits total runtime to six
hours, and never enables monthly billing, backups, or extra volumes. This is an
estimated compute guard, not a billing cap: it does not query your credit
balance or include unprovided ancillary charges, and an incorrect price gives
an incorrect estimate. Keep a separate account-level record when running many
seeds against a monthly credit allowance.

Normal completion, failures, SIGINT, and SIGTERM trigger bounded cleanup using
a fresh context even when the run timed out. Instance deletion is checked by
listing until the run's instances disappear, then the temporary SSH key is
removed. Cleanup handles lost creation responses by reconciling the exact names
recorded before creation. Existing name collisions are rejected.

SIGKILL, loss of the runner, or a provider/API outage can leave billable VMs.
Use the persisted manifest to retry cleanup; this does not need the SSH key or
an hourly-rate flag:

```sh
go run ./cmd/testvm -provider ovh -ovh-action destroy \
  -ovh-endpoint ovh-ca \
  -ovh-manifest artifacts/e2e-vm/<run-id>/ovh-resources.json
```

The endpoint must match the manifest. Inspect the OVH console if the API cannot
confirm deletion. Do not remove a run's manifest until cleanup is confirmed.

The provider request shapes follow the
[OVH Public Cloud API schema](https://eu.api.ovh.com/1.0/cloud.json), with signing
handled by [OVH's Go SDK](https://github.com/ovh/go-ovh).
