# Durable bounded logs

The log path carries five types: runtime, build, deploy, http, and
network. It is a bounded, at-least-once pipeline from container and
build output into ClickHouse (`service_logs`, `service_log_gaps`). The
shared primitives live in `internal/logpipeline`; the control-plane
store in `internal/controlplane/logs`. Console search surfaces are
backlog item 3.6; the read API contract for them is in
[frontend-handoff/2.9.md](frontend-handoff/2.9.md).

## Line contract

- A line at or under `MaxLogLineBytes` (64 KiB) is stored byte-exact.
  Longer lines keep the exact prefix and set `truncated`; the pipeline
  never splits or rewrites customer output.
- The pipeline never parses customer output. Structured `event` names
  and `attributes` travel only on lines the platform itself emitted
  (`build.started`, `build.finished`, `deploy.started`,
  `allocation.crash_loop`), with bounded attribute counts and sizes.
- Every line has a stable producer-assigned `line_id`:

  | Prefix | Producer | Identity |
  | --- | --- | --- |
  | `ag:` | agent | agent, boot, allocation, stream, sequence |
  | `bd:` | builder | builder, build, lease epoch, sequence |
  | `sy:` | control plane | content-stable event facts |

  The agent boot ID lets sequence counters restart at 1 per process
  without colliding. The builder lease epoch scopes a build attempt:
  a retried report reuses its IDs while a retried attempt writes under
  a new epoch. Synthetic event IDs derive from the content-stable
  facts of the observation (ownership, rollout generation, restart
  window) instead of wall clock, so an agent resending the same
  observation after a reconnect — or a duplicated status report —
  collapses into one event row under the same (observed_at, line_id)
  dedup that serves log retries; `allocation.crash_loop` fires once
  per restart window per allocation, not once per report. Conditions
  without restart timestamps cannot anchor an onset and fall back to
  a per-emission identity.

## Ordering and duplicate handling

Reads order by `(observed_at, line_id)`. Delivery is at-least-once:
producers commit their spool cursor only after the batch is accepted,
so a reconnect or a retry can resend lines. `service_logs` is a
`ReplacingMergeTree(ingested_at)` keyed by `(service_id, observed_at,
line_id)` and reads use `FINAL`, so resends collapse into one row
instead of duplicating. On session attach an agent rewinds a bounded
recent window (default 5 minutes) to re-send records the backend may
not have durably ingested; records after the durable cursor were
never accepted and always replay, however old. Retried lines
deduplicate by identity.

## Producer pipelines (agents and builders)

Container output funnels through a per-allocation token bucket
(default 200 lines/s, burst 1000) into a bounded disk-backed FIFO
spool (agent default 256 MiB under the runtime data dir; builders use a
per-attempt spool under the work dir, default 64 MiB). A ship loop
forwards batches with retry and exponential backoff; the spool cursor
commits only after acceptance. Nothing on this path blocks workload
reconciliation or build execution: log shipping failure costs only log
latency and, past the spool cap, dropped lines.

Every shed line is counted and reported with the next batch as a drop
summary, keyed per allocation (or stream for builders), and persisted
as an explicit gap row. Shutdown drains the counters into pending
drop summaries persisted next to the spool, so they report after the
restart. Spool records lost to crash corruption are counted the same
way with reason `corrupt_spool`, attributed best-effort from the
damaged frame's key (frames whose key bytes are gone stay in the
process counters only). Drop summaries carry the allocation as their
identity; the service is derived from the allocation owner at ingest.
Build spools are removed on clean completion and leftovers are
garbage-collected at startup; a retried attempt re-emits its own
output from scratch.

## Control-plane ingest

Batches enter a bounded in-memory queue (default 512 batches) behind a
per-allocation ingest guard (default 2000 lines/s, burst 10000) so a
buggy or hostile agent cannot starve ClickHouse. The flush loop retries
with backoff across a backend outage; only process shutdown drops the
backlog (producers then replay their unshipped spool). Queue overflow
sheds whole batches with owed gap rows so the loss still surfaces in
reads; past the owed-gap key cap shed windows fold into service-level
aggregate gaps rather than vanishing. Batches over 2000 entries are trimmed with the tail counted as
ingest gaps per affected service and allocation.
Ingest is per replica: each replica flushes the agent streams it
terminates, and the agent Sync loop never waits on ClickHouse.

## Gaps

A gap row records `allocation_id`/`build_id`, log type, stream, window,
`dropped_count`, a bounded reason (`rate_limited`, `spool_overflow`,
`ingest_overflow`, `corrupt_spool`), and the reporter. Gap identity
derives from the window contents so retried reports collapse instead of
double counting. Reads return the gaps overlapping the queried range
alongside lines, and every shed point counts drops — a dropped window
is always an explicit gap, never silently closed. Gap attribution is
derived from the allocation (or build) owner, never from producer
claims: a claimed service that does not own the allocation rejects
the batch, an empty claim is filled from the owner, and reports with
no allocation cannot be attributed and never become gap rows.

## Retention and safe deletion

Each line and gap row carries `expires_at` from the owning project's
`log_retention_days` (1..90; 0 means the platform default, 14 days)
resolved at write time, enforced by ClickHouse row TTL. Projects can
set the policy via `UpdateProjectLogRetention`; rows written before a
policy change keep their original expiry. When the deletion GC
collects an expired project tombstone it purges that project's lines
and gaps synchronously (`PurgeProjectLogs`); per-row TTL expiry is the
backstop if the purge is lost. Reads are authorized per service before
querying, so tenant isolation follows the existing project membership.

## Configuration

| Component | Flag / env | Default |
| --- | --- | --- |
| agent | `logs-spool-max-bytes` / `AGENT_LOGS_SPOOL_MAX_BYTES` | 256 MiB |
| agent | `logs-rate-per-sec`, `logs-burst` / `AGENT_LOGS_RATE_PER_SEC`, `AGENT_LOGS_BURST` | 200/s, 1000 |
| agent | `logs-flush-batch-size`, `logs-flush-interval-seconds` / `AGENT_LOGS_FLUSH_BATCH_SIZE`, `AGENT_LOGS_FLUSH_INTERVAL_SECONDS` | 100, 1s |
| agent | `logs-replay-window-seconds` / `AGENT_LOGS_REPLAY_WINDOW_SECONDS` | 300 |
| builder | `logs-spool-max-bytes` / `BUILDER_LOGS_SPOOL_MAX_BYTES` | 64 MiB |
| builder | `logs-rate-per-sec`, `logs-burst` / `BUILDER_LOGS_RATE_PER_SEC`, `BUILDER_LOGS_BURST` | 200/s, 1000 |
| builder | `logs-flush-batch-size`, `logs-flush-interval-seconds` / `BUILDER_LOGS_FLUSH_BATCH_SIZE`, `BUILDER_LOGS_FLUSH_INTERVAL_SECONDS` | 100, 1s |
| control plane | `logs-clickhouse-url` / `CONTROLPLANE_LOGS_CLICKHOUSE_URL` | unset (log storage disabled) |
| control plane | `logs-retention-days` / `CONTROLPLANE_LOGS_RETENTION_DAYS` | 14 |
| control plane | `logs-ingest-queue-flushes` / `CONTROLPLANE_LOGS_INGEST_QUEUE_FLUSHES` | 512 |
| control plane | `logs-ingest-rate-per-sec`, `logs-ingest-burst` / `CONTROLPLANE_LOGS_INGEST_RATE_PER_SEC`, `CONTROLPLANE_LOGS_INGEST_BURST` | 2000/s, 10000 |

A non-positive producer rate disables producer-side limiting (the
ingest guard still applies). Zero values elsewhere select the defaults
above.

## Schema cutover

`service_logs` previously used a plain `MergeTree` without line
identity, tenant attribution, or per-row retention. On first boot
against such a table the control plane drops it and recreates the
ReplacingMergeTree schema; the pre-2.9 rows are lost deliberately, as
telemetry is not customer authority and the row semantics are
incompatible. `service_log_gaps` is created alongside.
