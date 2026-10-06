# Compact resource benchmarks

Measured on Linux amd64, AMD Ryzen 7 2700X, Go 1.27.1. Docker workloads use
explicit CPU/memory limits. Microbenchmarks run three times; report the median.
Cgroup working-set RAM is `memory.current - inactive_file`, not summed process
RSS (which double-counts shared pages). CPU is cgroup usage time / wall time,
where 100% means one fully used core. The short container samples include the
small cost of `docker exec` probes and post-ingestion maintenance. They are
neither steady-state idle measurements nor a production traffic forecast.

## 1. Optional ClickHouse

100 lines, 200 payload bytes each, stable line IDs, 100 writes per run. Both
backends acknowledge a durable write. File logs include fsync and directory sync.
ClickHouse uses `clickhouse/clickhouse-server:25.3`, capped at two CPUs and 1.5 GiB.

| Metric per 100-line batch | ClickHouse client | File backend | Change |
| --- | ---: | ---: | ---: |
| Median allocated bytes | 465,867 | 175,636 | 62.3% less |
| Median elapsed time | 7.95 ms | 6.99 ms | 12.0% less |
| Allocations | 2,706 | 873 | 67.7% less |

The removed ClickHouse server used a median **243.6 MiB** working set and
**9.18% of one CPU core** during three five-second samples after ingest.
This server cost is separate from the Go client work above. Removal does not
mean the same percentage CPU saving under a different query/ingest workload.
The registry reference used 9.48 MiB in the same sampling method.

Raw data: [logs.txt](logs.txt), [log-containers.json](log-containers.json).

Reproduce the microbenchmark with a disposable, reachable ClickHouse:

```sh
COMPACT_BENCH_CLICKHOUSE_URL=clickhouse://127.0.0.1:19000/default \
  go test ./internal/controlplane/logs -run '^$' \
  -bench BenchmarkLogBackend -benchtime=100x -count=3
python3 scripts/compact-container-sample.py compact-bench-clickhouse
```

The benchmark skips ClickHouse when its environment variable is absent.

## 2. Local durable storage

Read a verified 1 MiB archive range 100 times, three runs. Both adapters start
with the same content-addressed object. S3 uses an authenticated loopback HTTP
fixture; this measures the adapter and HTTP work, without WAN latency or the
RAM/CPU cost of an external S3 server.

| Metric | S3 adapter | Disk adapter | Change |
| --- | ---: | ---: | ---: |
| Median allocated bytes/read | 2,258,927 | 1,049,145 | 53.6% less |
| Median elapsed time/read | 1.632 ms | 0.372 ms | 77.2% less |
| Allocations/read | 254 | 6 | 97.6% less |
| Peak benchmark-process RSS, 1,000 reads | 31,628 KiB | 23,200 KiB | 26.6% less |
| Process CPU time, 1,000 reads | 3.85 s | 1.05 s | 72.7% less |

The process figures use `/usr/bin/time` on the compiled Go test binary and include
fixture work. They do not include a production S3 server. Allowing a local registry
address alone has **no measured RAM/CPU reduction**: the same registry still runs.
The change removes a deployment restriction. Its disk must still be backed up.

Raw data: [archive.txt](archive.txt), [disk process](archive-file-process.txt),
[S3 process](archive-s3-process.txt).

```sh
go test ./internal/controlplane/source -run '^$' \
  -bench BenchmarkArchiveRead -benchtime=100x -count=3
CONTROLPLANE_TEST_DATABASE_URL=postgresql://USER:PASSWORD@127.0.0.1:5432/testdb?sslmode=disable \
  go test -tags=integration ./internal/controlplane ./internal/sqlretry
```

The database URL must name a disposable test database; the control-plane fixture
creates isolated databases on that server. Run the dashboard integration tests
against PostgreSQL with `DASHBOARD_TEST_DATABASE_URL` set to that test database.

## 3. Idle and active builds

The startup benchmark uses a real containerd backend and a pre-pulled BusyBox
image in the privileged Linux runtime fixture. The eager case is the previous
constructor path: connect, inspect the image, and unpack it. The new constructor
creates only an executor description. No build runs in either case.

| Metric per startup | Previous eager setup | On-demand setup | Change |
| --- | ---: | ---: | ---: |
| Median allocated bytes | 470,972 | 704 | 99.85% less |
| Median elapsed time | 14.11 ms | 0.000992 ms | 99.99% less |
| Allocations | 4,671 | 1 | 99.98% less |

These are startup allocation/work savings, **not idle RSS or CPU-time savings**.
The startup work moves to the first claimed build. Sandbox and BuildKit daemons
were already per-build, so their additional idle saving is **zero**. Capacity and
preference tests show that a small VM claims no oversized build, that a capable
home builder gets priority, and that a fenced takeover follows power loss. That
keeps build memory and CPU off the VM; it does not reduce the build's total work.

Raw data: [builder-startup.txt](builder-startup.txt).

```sh
./scripts/test-linux-runtime-docker.zsh bash -c \
  'ctr -n compact-bench images pull docker.io/library/busybox:1.36.1 &&
   COMPACT_BENCH_SANDBOX=1 go test ./internal/builder -run "^$" \
   -bench BenchmarkHardenedStartup -benchtime=10x -count=3'
```
