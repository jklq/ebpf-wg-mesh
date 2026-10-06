# Compact deployment

The target is one always-on Linux VM with 2 GiB of RAM, PostgreSQL, disk-backed
archives and registry data, bounded file logs, and one service with a 256 MiB
memory limit. Builds can run on an intermittent machine, such as a home PC.

## Logs

Without `CONTROLPLANE_LOGS_CLICKHOUSE_URL`, the control plane keeps JSONL files
under `CONTROLPLANE_STATE_DIR/logs`. Set `CONTROLPLANE_LOGS_DIR` to change the
location. `CONTROLPLANE_LOGS_MAX_BYTES` defaults to 64 MiB across all projects;
`CONTROLPLANE_LOGS_SEGMENT_BYTES` defaults to 1 MiB. The oldest segments are
removed when the byte budget is reached. A write batch may form a smaller segment.

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
