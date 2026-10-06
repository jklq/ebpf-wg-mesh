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
