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
