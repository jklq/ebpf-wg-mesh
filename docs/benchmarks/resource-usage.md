# Resource usage benchmark

Measured 2026-10-16 on the development host against PostgreSQL 17 (Docker,
via `internal/testdb`), with `TestUsageIngestThroughput1000Allocations`:

- Ingest: 10,000 records across 1,000 allocations in 1.53 s
  (6,546 records/s, 153 µs/record) in full 500-record reports.
- At 1,000 allocations sealing one record per minute, production load is
  1.44M records/day ≈ 17 records/s average — roughly 0.3% of measured
  single-threaded ingest capacity.
- Query: 30 days at hourly resolution over those 1,000 resources returned
  721 buckets in 137 ms.

Reproduce:

```sh
go test -count=1 -tags integration ./internal/controlplane -run TestUsageIngestThroughput1000Allocations -v
```
