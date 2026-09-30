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
  per restart window per allocation, not once per report. Build
  lifecycle events derive identity and observed_at from the build,
  lease attempt, and recorded event time the same way: a retried
  claim or completion report collapses into one `build.started` or
  `build.finished` row. Conditions
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
not have durably ingested; the rewind is serialized against in-flight
flushes, so a batch read before it cannot commit past it and swallow
the replay window. Records after the durable cursor were
never accepted and always replay, however old. Retried lines
deduplicate by identity.

## Producer pipelines (agents and builders)

Container output passes through per-allocation and aggregate token buckets
into a durable FIFO (agent default 256 MiB under the runtime data directory;
builders default 64 MiB per attempt under the work directory). The FIFO is
one bbolt database, `logs.db`, containing records, the acknowledgement cursor,
and pending drop summaries. Each append, eviction, and acknowledgement uses a
synced transaction. There is no segment framing, checksum repair, cursor file,
or separate pending-drop file.

`MaxBytes` bounds retained encoded record bytes, including attribution and
payload encoding, and `MaxRecords` independently caps record count (default
1,000,000). bbolt's `MaxSize` enforces a separate physical file limit of
`4 * MaxBytes + 8 MiB`, allowing pages, copy-on-write transactions, and drop
metadata. Deleted pages are reused; the file retains its high-water size.
Drop identities consume that same hard disk budget and never fold across
allocations or services. If an append or loss-accounting transaction cannot
fit, it fails without deleting the prior records or accounting. The agent
reports that persistence failure loudly; a builder fails the attempt rather
than completing with unaccounted output.

Shipping is at least once. A read pins only the returned records against
eviction while the network call runs. Unread records can be shed under
pressure; every such deletion atomically adds an exactly attributed loss
window. When all capacity is pinned, admission fails. Successful delivery
atomically advances the cursor and acknowledges the delivered drop snapshot;
a storage failure replays both. Released or stale read tokens cannot
acknowledge a later batch. Network delivery never holds a database transaction.

Line batches and drop snapshots use separate byte-bounded messages below the
transport's receive limit, so large lines or accumulated gap identities
cannot wedge delivery. Agent acceptance is a `LogBatchAck` after the complete
batch commits to the control plane's durable ingest journal. Agents retain
acknowledged records for a recent replay window (default five minutes), and
serialize reconnect rewinds with flushes. Replay copies yield to unaccepted
output under pressure without being reported as losses. Builders need no
retention: their report RPC writes durably before returning.

Producer denials and failed admissions persist their drop windows immediately,
even while detached. Windows with the same allocation or build, stream,
log type and reason coalesce under a stable summary ID. Failed sends leave the
snapshot durable with its existing IDs. Losses arriving during a successful
send remain pending under a fresh ID after acknowledgement, so a reduced
count never overwrites an already delivered window. An undecodable producer
payload becomes an exactly attributed `corrupt_spool` gap in the same
transaction that discards it; subsequent records remain deliverable. Torn
bbolt commits recover through its transaction machinery. A damaged database
is refused explicitly; there is no best-effort page salvage or silent reset.

Build output readers use a bounded in-memory channel before disk writes, so
normal output never waits for a per-line fsync. Close drains that channel,
ships at least one final round (including gap-only attempts), and returns an
error if the backend has not accepted the transcript within the close
deadline. A lease loss or abandonment converts unaccepted stored records to
per-stream gaps transactionally. A later lease imports dead attempts' gap
snapshots with their stable IDs, then acknowledges and removes the source
only after the import is durable. Repeating an interrupted takeover replaces
the snapshot instead of adding its counts again. A retry re-emits its own
output under its new epoch and never ships the old attempt's lines.
Clean completion removes the drained database; stale attempt directories
are collected after 24 hours without a write, following database modification
time rather than the directory's creation time.

## Control-plane ingest

Batches enter a durable ingest journal (default 64 MiB of retained
payload — line text, retained IDs, and attribute maps all count)
behind a
per-allocation ingest guard (default 2000 lines/s, burst 10000) so a
buggy or hostile agent cannot starve ClickHouse — gap rows consume
the same guard one token each, so gap-only spam cannot add retained
rows past the limit, and a denied report is never erased: its
accounting coalesces per window (summary-identified reports keep
their replace semantics) and rides the batch's own journal record,
durable before the batch is acknowledged. Batches are scoped
line by line to the allocations the agent owns: a line for a stale
allocation — one removed while its lines sat in the durable spool —
or carrying a mismatched claim is excluded with a warning instead of
rejecting the whole batch, so one bad line can never wedge durable
delivery behind it (excluded lines carry no verifiable tenant and
therefore no gap row). The flush loop drains the journal in bounded
rounds and retries with backoff across an outage; because the
journal is durable, an accepted batch survives ClickHouse outages
that outlast the replay window and control-plane restarts — only a
lost journal directory can drop it. Each replica keeps its own
journal directory inside the shared state volume, so failover never
races two drainers over one journal. Journal overflow
sheds a complete batch only when its exact gap accounting fits durably;
otherwise admission returns retry and the producer retains its copy. Batches over 2000 entries are trimmed with the tail counted as
ingest gaps per affected service and allocation.
Shutdown drains the journal under a 15s grace deadline:
the batch caught mid-retry and journaled line and gap batches
flush before the process exits,
even when the run context is already canceled, and the server waits
for the drain before closing the log store so every flush runs
against a live backend. gRPC streams stop before the drain, and
admission seals when the drain starts: a
batch racing the seal is rejected and loudly accounted while the
producer's retained replay window
re-sends it on reconnect. What the grace cannot write stays
journaled and lands after the next boot; post-seal arrivals fall
back to synchronous writes or producer replay.
Platform events (build/deploy lifecycle, crash loops) ride the same
durable journal: retry across outages, shed with gap accounting,
drain at shutdown.
Ingest is per replica: each replica flushes the agent streams it
terminates, and the agent Sync loop never waits on ClickHouse.

## Gaps

A gap row records `allocation_id`/`build_id`, log type, stream, window,
`dropped_count`, a bounded reason (`rate_limited`, `spool_overflow`,
`ingest_overflow`, `corrupt_spool`), and the reporter. Gap identity
keys on the producer's stable summary ID when the gap carries one:
retried reports of the same coalesced drop lineage collapse onto one
row even after their totals or window grew, so retries never double
count. Internally derived gaps are immutable per event and key on
their full content. The same identity keeps shed reports honest:
a replayed producer summary keeps its ID in the journal and at the sink,
so its repeated reports replace the same gap row. Reads return the gaps overlapping the queried range
alongside lines, paginated with their own opaque cursor
(`gap_page_token`/`next_gap_page_token`, oldest first by window) so a
range with more rows than one response may carry keeps every gap
reachable, and every shed point counts drops — a dropped window
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
backstop if the purge is lost. Rows that carry neither explicit
attribution nor a resolver entry are refused at write time and
counted loudly: after a service or project is hard-deleted, late
flushes must not write unattributable rows under the platform default
TTL that the deletion purge cannot find and a short project policy
would not bound. Reads are authorized per service before
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
| control plane | `logs-ingest-queue-bytes` / `CONTROLPLANE_LOGS_INGEST_QUEUE_BYTES` | 64 MiB |
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

## Local spool format cutover

This release replaces the segmented local spool with `logs.db` everywhere:
agents, builder attempts, and control-plane ingest journals. Opening a
directory containing old segments or cursor/drop JSON files fails with an
explicit format error. Drain those directories using the previous release,
then remove them before deploying this release, or deliberately reset their
contents if losing the old local telemetry is acceptable. There is no format
migration or compatibility reader. Existing ClickHouse line/gap tables and
producer identities remain the same.
