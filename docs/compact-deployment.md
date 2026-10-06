# Compact deployment

The target is one always-on Linux VM with 2 GiB of RAM, PostgreSQL, disk-backed
archives and registry data, bounded file logs, and one service with a 256 MiB
memory limit. Builds can run on an intermittent machine, such as a home PC.

## Logs

Without `CONTROLPLANE_LOGS_CLICKHOUSE_URL`, the control plane keeps JSONL files
under `CONTROLPLANE_STATE_DIR/logs`. Set `CONTROLPLANE_LOGS_DIR` to change the
location. `CONTROLPLANE_LOGS_MAX_BYTES` defaults to 64 MiB across all projects;
`CONTROLPLANE_LOGS_SEGMENT_BYTES` defaults to 1 MiB. The oldest segments are
removed when the byte budget is reached. Small writes share a segment; each completed batch is synced to disk.

`CONTROLPLANE_LOGS_RETENTION_DAYS` defaults to 14. Project-specific retention
still applies. Expired rows disappear from queries immediately, and a sweep
removes them from disk within one minute. Reads keep only one page in memory,
scan the retained files, and support search, time filters, and pagination.
This is suitable for basic logs with low volume. A high query rate increases
CPU and disk use. Use ClickHouse for larger log volumes or frequent searches.

The file directory has one writer. Multiple control-plane replicas should use
ClickHouse. Selecting ClickHouse uses that backend for new logs; existing file
history stays on disk until the operator removes or exports it. The durable
ingest journal and producer spools still record overload and lost-log gaps.

The local development stack uses file logs by default. Set
`LOCALTESTSTACK_ENABLE_CLICKHOUSE=1` to start its optional ClickHouse container.

## Measurements

See [benchmarks/compact.md](benchmarks/compact.md). RAM values are measurements,
not a guarantee for every workload. Allocation bytes measure temporary work;
they are not the same as resident RAM.

## Durable local storage and PostgreSQL

Set `CONTROLPLANE_STATE_DIR=/var/lib/ebpf-wg-mesh/controlplane` on a persistent
filesystem. The file archive provider works in production. It defaults to the
`source-archives` directory below the state directory. Use
`CONTROLPLANE_SOURCE_ARCHIVES_DIR` for a separate persistent disk.
Relative paths, `/tmp`, `/var/tmp`, `/run`, and `/dev/shm` are rejected in production.
Local file storage does not provide replication. Keep a backup on another machine.

The control-plane schema and dashboard schema use PostgreSQL SQL types.
Transactions use serializable isolation and retry the whole transaction on
serialization conflicts and deadlocks. CockroachDB remains supported by the
same SQL. This is a flat schema cutover: recreate a database with an older
control-plane schema version rather than applying a migration chain.

The production database, registry, and optional ClickHouse can use loopback.
Keep mTLS identities and the public HTTPS ingress settings. A loopback registry
works for a builder and agent on the same VM. A home builder must use a registry
address it can reach, such as an authenticated HTTPS endpoint or a VPN address.
The operator's local registry must retain its data on a persistent filesystem.
The PaaS authenticates the registry; it does not run the registry storage process.

Back up PostgreSQL with `pg_dump`, and copy the source archives and registry
storage to another machine. Back up the control-plane state and provisioned
keyring separately from the database. The encrypted database alone cannot
recover service secrets or signing keys. Test a restore into a fresh database
and disk directory before relying on the backups.

## Intermittent hosts and builds

Choose **Intermittent (builds only)** when creating or editing a fleet agent.
The protocol field is `host_type=intermittent`. Normal application placement,
including volumes, uses stable agents. Drain existing allocations before changing
a stable agent to intermittent. The host type survives disconnects and restarts.

Run one builder per machine. On the home PC, set `BUILDER_HOST_TYPE=intermittent`.
Keep its state, mTLS credentials, toolchain image, and work directory across restarts.
The VM can run a stable builder as fallback, or omit the builder entirely.
Builder client certificates are still operator-provisioned. Their default validity
is 24 hours. After a longer shutdown, refresh the builder certificate and CA files
before starting it. `CONTROLPLANE_INTERNAL_CLIENT_CERT_VALIDITY_HOURS` can set a
validity window that covers the expected offline period; it applies to internal
client certificates generally. Agent certificate recovery uses its retained
bootstrap token and private key. Neither behavior changes the build lease rules.
Online, idle, capable intermittent builders have priority. A busy or stale home
builder does not prevent a capable stable builder from claiming work.

Every offer reports free memory, cgroup v2 memory headroom, CPU quota/affinity,
and the resources required for one build. The defaults keep 256 MiB free for
other work and require one CPU core. The default build step memory limit is
8 GiB; another 512 MiB covers BuildKit headroom. Such a build waits on a 2 GiB VM.
It can run when the home PC has enough free capacity. These values are tunable:
`BUILDER_RESERVE_MEMORY_BYTES`, `BUILDER_RESERVE_CPU_MILLIS`,
`BUILDER_BUILD_CPU_MILLIS`, `BUILDER_DAEMON_MEMORY_BYTES`, and
`BUILDER_BUILD_MEMORY_BYTES` (see `builder -h` for the exact limit flag).
Admission does not reserve memory against unrelated processes on the home PC.
Keep the configured reserve large enough for your desktop and other work.

An idle builder opens no containerd client and unpacks no toolchain image.
Each claimed build opens its backend, starts its sandbox and BuildKit daemon,
and releases them afterward. Workspace recovery also opens a backend only when
leftover execution state needs it. BuildKit was already per-build before this
change; the new saving is deferred backend setup and keeping active builds off
the small VM.

No capacity means queued work, with no claimed attempt. Queued builds wait
indefinitely by default, including when the home PC stays off overnight.
`CONTROLPLANE_BUILDER_MAX_QUEUE_AGE_SECONDS` can set an explicit queue deadline.
Running builds keep the existing lease, timeout, and attempt limits. A powered-off
builder loses its lease; a capable builder can retry the work with a new fenced
owner epoch. Repeated interruptions can exhaust the attempt limit.

## Small pools and a 2 GiB resource plan

The default control-plane pool is 8 open connections and 2 idle. It no longer
scales up with CPU count. Use `CONTROLPLANE_DB_MAX_OPEN_CONNS` and
`CONTROLPLANE_DB_MAX_IDLE_CONNS` to grow it. The dashboard uses 2 connections.
Optional ClickHouse uses 2 open / 1 idle, controlled by
`CONTROLPLANE_LOGS_CLICKHOUSE_MAX_OPEN_CONNS` and
`CONTROLPLANE_LOGS_CLICKHOUSE_MAX_IDLE_CONNS`. Small pools queue bursts safely;
they can increase request latency under load.

For a two-vCPU, 2 GiB host, use this resource portion of the existing production
configuration. Add the normal bootstrap identities, public TLS/ingress settings,
registry authentication, and dashboard credentials. Keep the database password
in the protected service environment, not in a checked-in file.

```sh
CONTROLPLANE_PROFILE=production
CONTROLPLANE_STATE_DIR=/var/lib/ebpf-wg-mesh/controlplane
CONTROLPLANE_DB_URL=postgresql://platform:PASSWORD@127.0.0.1:5432/platform?sslmode=disable
CONTROLPLANE_DB_MAX_OPEN_CONNS=8
CONTROLPLANE_DB_MAX_IDLE_CONNS=2
CONTROLPLANE_LOGS_MAX_BYTES=67108864
CONTROLPLANE_LOGS_SEGMENT_BYTES=1048576
CONTROLPLANE_LOGS_RETENTION_DAYS=14

AGENT_MEMORY_MEBIBYTES=2048
AGENT_RESERVED_MEMORY_MEBIBYTES=1536
AGENT_CPU_MILLIS=2000
AGENT_RESERVED_CPU_MILLIS=1000
AGENT_DATA_DIR=/var/lib/ebpf-wg-mesh/agent
```

Leave `CONTROLPLANE_LOGS_CLICKHOUSE_URL` unset. Do not run a builder on the VM
unless it has capacity for the configured build plus daemon headroom. The
512 MiB schedulable budget accommodates a 256 MiB managed dashboard and one
256 MiB application. Keep the dashboard and application's combined CPU requests
within the remaining 1,000 CPU millis.

A small PostgreSQL starting configuration is:

```conf
shared_buffers = 32MB
work_mem = 1MB
maintenance_work_mem = 16MB
max_connections = 24
autovacuum_max_workers = 1
```

This leaves room for the control plane, dashboard, an application's small pool,
and administration. `work_mem` applies to each query operation, so it is not a
single global buffer. Give application pools explicit limits too. Keep autovacuum
on, and watch query latency and database growth before raising these values.
Use one PostgreSQL server with separate databases/schemas and roles as needed.
Do not run a second database just for the dashboard.

The passing capped smoke test used about 390 MiB of working-set RAM, with a
256 MiB application limit and 192 MiB of touched application data. It did not
include the console UI or a separate VM kernel, and shared one process between
control plane and agent. Reserve space for those costs. See the full measurement
limits and reproduction commands in [the benchmark report](benchmarks/compact.md).
This supports the 2 GiB target for small traffic. It does not establish a safe
lower VM size or a high-traffic capacity guarantee.
