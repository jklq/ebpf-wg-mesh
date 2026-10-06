# Compact resource benchmarks

Measured on Linux amd64, AMD Ryzen 7 2700X, Go 1.27.1. Docker workloads use
explicit CPU/memory limits. Microbenchmarks run three times; report the median.
Cgroup working-set RAM is `memory.current - inactive_file`, not summed process
RSS (which double-counts shared pages). CPU is cgroup usage time / wall time,
where 100% means one fully used core. The short container samples include the
small cost of `docker exec` probes and post-ingestion maintenance. They are
neither steady-state idle measurements nor a production traffic forecast.

## 1. Optional ClickHouse

100 lines, 200 payload bytes each, stable line IDs, 1,000 writes per run. Both
backends acknowledge a durable write. File logs include fsync and directory sync.
ClickHouse uses `clickhouse/clickhouse-server:25.3`, capped at two CPUs and 1.5 GiB.

| Metric per 100-line batch | ClickHouse client | File backend | Change |
| --- | ---: | ---: | ---: |
| Median allocated bytes | 464,862 | 112,067 | 75.9% less |
| Median elapsed time | 7.83 ms | 4.81 ms | 38.6% less |
| Allocations | 2,705 | 402 | 85.1% less |

The removed ClickHouse server used a median **243.6 MiB** working set and
**9.18% of one CPU core** during three five-second samples after ingest.
This server cost is separate from the Go client work above. Removal does not
mean the same percentage CPU saving under a different query/ingest workload.
The registry reference used 9.48 MiB in the same sampling method.

In a separate 1,000-batch process run, file writes used 23,328 KiB peak RSS
and 1.34 CPU-seconds. The ClickHouse client used 25,628 KiB and 1.81 CPU-seconds:
9.0% less peak process RAM and 26.0% less process CPU for files. Server work is
excluded from these process figures. Fsync latency varies with concurrent disk
work; this single process run took 9.61 seconds for files and 8.70 for ClickHouse.
The repeated microbenchmark above ran under different disk load.

Raw data: [final log batches](logs-final.txt), [log-containers.json](log-containers.json),
[file process](log-file-process.txt), [ClickHouse client process](log-clickhouse-process.txt).
The [initial implementation run](logs.txt) is retained for the per-commit record.

Reproduce the microbenchmark with a disposable, reachable ClickHouse:

```sh
COMPACT_BENCH_CLICKHOUSE_URL=clickhouse://127.0.0.1:19000/default \
  go test ./internal/controlplane/logs -run '^$' \
  -bench BenchmarkLogBackend -benchtime=1000x -count=3
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

In a separate 1,000-startup process benchmark under the two-CPU cap, eager
setup used 36,284 KiB peak RSS and 9.10 CPU-seconds. On-demand setup used
30,608 KiB and 0.01 CPU-seconds: 15.6% less peak process RAM and about 99.9%
less startup CPU. The 0.01-second result is at the timer's reporting resolution.
Containerd's own CPU work is excluded from these process figures.

These are startup savings, **not a steady-idle RSS/CPU measurement**.
The startup work moves to the first claimed build. Sandbox and BuildKit daemons
were already per-build, so their additional idle saving is **zero**. Capacity and
preference tests show that a small VM claims no oversized build, that a capable
home builder gets priority, and that a fenced takeover follows power loss. That
keeps build memory and CPU off the VM; it does not reduce the build's total work.

Raw data: [builder-startup.txt](builder-startup.txt),
[eager process](builder-eager-process.txt), [on-demand process](builder-on-demand-process.txt),
[eager run](builder-eager-run.txt), [on-demand run](builder-on-demand-run.txt).

```sh
./scripts/test-linux-runtime-docker.zsh bash -c \
  'ctr -n compact-bench images pull docker.io/library/busybox:1.36.1 &&
   COMPACT_BENCH_SANDBOX=1 go test ./internal/builder -run "^$" \
   -bench BenchmarkHardenedStartup -benchtime=10x -count=3'
```

## 4. Smaller pools and buffers

Use PostgreSQL 17.11 with `shared_buffers=32MB`, `work_mem=1MB`,
`maintenance_work_mem=16MB`, `max_connections=40`, two CPUs, and a 256 MiB
container limit. Saturate each pool, return the connections, then take three
five-second samples. The old case uses 32 open / 16 idle connections, which was
the previous minimum. The new case uses 8 open / 2 idle. The same server is
sampled in sequence; the second sample retains the first run's database cache.

| Metric | Previous pool | Compact pool | Change |
| --- | ---: | ---: | ---: |
| PostgreSQL median working set | 131.2 MiB | 110.6 MiB | 20.6 MiB / 15.7% less |
| Median CPU use, one core | 1.128% | 1.117% | No meaningful measured saving |
| 32 simultaneous 10 ms queries | 29.5 ms | 52.4 ms | More waiting; all succeed |

The control-plane pool no longer grows with CPU count. Idle connections expire
after one minute. The dashboard pool falls from the `pg` default of 10 to 2,
with a 30-second idle timeout. Optional ClickHouse uses 2 open / 1 idle.
The database measurement covers the control-plane pool only; it does not
assign an unmeasured RAM saving to the dashboard or ClickHouse pool.

Journal flushes read one record instead of eight. A record's estimated payload
target falls from 2 MiB to 256 KiB, and its row target falls from 5,000 to 500.
That reduces the nominal payload target per flush from 16 MiB to 256 KiB,
a 98.4% reduction. JSON encoding, decoded objects, and one oversized row can
exceed the estimate; it is not a hard resident-memory limit. The retained
64 MiB ingest journal is on disk. Build output scanners start with 4 KiB
instead of 64 KiB, and can still grow to handle 256 KiB lines.

File writes now append to full segments and track segment metadata. The initial
one-file-per-batch implementation used 9.35 CPU-seconds for 1,000 batches; the
final version uses 1.34, an 85.7% reduction. The raw initial run is
[here](log-file-initial-process.txt). No full-history row index is held
in RAM. Queries retain one page, but still decode and scan the bounded history.

Raw data: [pools.json](pools.json), [file process](log-file-process.txt).

```sh
go test -c -tags=integration ./internal/controlplane -o /tmp/controlplane.test
CONTROLPLANE_TEST_DATABASE_URL=postgresql://USER:PASSWORD@127.0.0.1:5432/testdb?sslmode=disable \
  python3 scripts/benchmark-compact-pools.py /tmp/controlplane.test POSTGRES_CONTAINER
```

## Compact stack check

The smoke test puts PostgreSQL 15, the registry, Envoy, containerd, the control
plane, the agent, WireGuard/eBPF policy, and one real Python application in a
single Linux cgroup. It has a **2 GiB hard limit, two CPUs, and no swap**.
The application's memory limit is 256 MiB. It touches and retains 192 MiB of
heap data, runs an HTTP server, passes its readiness check, and reaches an
active deployment. File logs accept application/platform events.

The control plane and agent share the Go test process. The profile is development,
with a directory volume backend. The console UI is excluded. PostgreSQL 17
also passes the full control-plane and dashboard integration suites separately.
This is a short low-traffic smoke test, not a production soak or a VM test with
its own kernel. Container RAM excludes the host kernel's full overhead.

The successful run measured **390.4 MiB median working-set RAM** and
**0.68% of one CPU core** in three five-second samples after startup.
The total cgroup memory peak was **1,062.3 MiB**, including package installation,
image pulls and file cache. `memory.events` reported zero limit hits, OOMs or
OOM kills. Peak total usage and post-start working set are different measures.

Reserve 1,536 MiB for the host and platform on a 2 GiB VM. Advertise only 512 MiB
to the scheduler: 256 MiB for the managed dashboard and 256 MiB for one app.
The application limit covers its allocation; it does not reserve 256 MiB of
physical RAM when the app is idle. Leave builds on the home PC. The measured
stack, plus a 256 MiB console budget, separate-process overhead and an OS reserve,
fits the target with room for small workloads. A conservative planning sum is
390 MiB measured stack + 256 MiB console + 64 MiB for separate processes +
45 MiB for the application's remaining allowance + 512 MiB for the OS:
about **1.25 GiB**, leaving roughly **0.75 GiB**. The added allowances are planning
reserves, not separately measured processes. Higher traffic, log queries,
images, database growth and backups must be measured on the actual VM.

Raw data: [stack.json](stack.json), [passing probe](stack-probe.txt).
Reproduce the capped check and startup CPU probes (requires Docker on Linux):

```sh
python3 scripts/benchmark-compact-stack.py /tmp/compact-stack-new
```

The script requires a new empty output directory. It compiles outside the cap,
starts disposable containers, samples the stack after readiness, and removes
its containers when done. Benchmark scripts print only disposable fixture data.

## Further gains to measure

- Bound Envoy downstream connections and right-size its worker count. The
  fixture explicitly uses two workers; high connection counts remain a memory risk.
- Right-size eBPF maps for compact nodes. Current defaults allow 1,024 containers,
  10,000 conntrack entries per container, and 65,536 cluster identities.
- Stream archive ranges to consumers. A 1 MiB disk read still allocates about
  1 MiB for the result; local storage removes HTTP overhead but not this copy.
- Reduce dashboard runtime memory and measure its actual peak under login and
  service management traffic. The current 256 MiB budget was reserved, not sampled.
- Reduce repeated control-plane projections and xDS snapshot construction as
  service count grows. The tiny-stack sample does not quantify that workload.
- Put a hard aggregate cgroup limit around each build and its daemon. Admission
  currently uses a build-step limit plus estimated daemon headroom; unrelated
  desktop processes can still consume memory after an offer.
