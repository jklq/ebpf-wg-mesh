// Package durablework is the single CockroachDB-backed durable work queue for
// control-plane background loops.
//
// It replaces the bespoke github_work_items, source_work_items, and
// github_webhook_deliveries tables with one small table and one lease protocol.
// It is not a workflow DSL: there are no DAGs, sub-tasks, timers, or sagas.
// A work record moves through pending -> leased -> one terminal state
// (succeeded, failed, dead), with bounded retries back to pending in between.
//
// # Record shape
//
// Every record carries a stable kind and ID, a unique deduplication key,
// scoped resource identity (resource type plus resource ID), state, attempt
// count and limit, owner ID, an owner epoch that increments on every claim,
// lease expiry with heartbeat, a sanitized last error, an available-at time
// for backoff, a JSON payload, and timestamps.
//
// # Enqueue with the product mutation
//
// EnqueueTx joins the caller's *sql.Tx so the work record commits atomically
// with the product-state mutation that requires it. A product change without
// its follow-up work, or follow-up work without its product change, must be
// unrepresentable; never enqueue in a separate transaction from the mutation.
//
// Enqueue deduplicates on the dedup key while a record is active
// (pending or leased) and resurrects it when the record is terminal
// (succeeded, failed, dead). Resurrection is the recovery path for dead
// letters: re-enqueueing the same dedup key resets the record to pending.
// Callers whose triggers are level-based (a reconciler that re-enqueues on
// every pass) therefore heal dead rows automatically; callers whose triggers
// are edge-based (a webhook) rely on the next edge or on an operator
// re-enqueue.
//
// # Fencing without a fencing protocol
//
// Claim, Heartbeat, Complete, and Fail are each a single compare-and-swap on
// (owner ID, owner epoch). Every claim increments the epoch, so when a lease
// expires and a second owner takes over, the first owner's epoch is stale and
// its late heartbeat, completion, or failure is rejected with ErrLeaseLost.
// A stalled owner cannot commit after takeover. No separate fencing token is
// layered on top; the (owner, epoch) pair is the fence.
//
// Leases expire; there is no explicit recovery scan. A worker that dies
// without completing simply lets its lease lapse, and the next Claim takes
// the record over with a bumped epoch and attempt count. Attempt count
// increments on every claim, including takeovers, so a record whose workers
// keep dying still converges to the dead state instead of retrying forever.
//
// # Retries and terminal states
//
// Fail takes a retryable flag. A non-retryable failure (a poison payload, an
// unknown kind) moves the record to failed immediately. A retryable failure
// with attempts remaining moves it back to pending with a jittered
// exponential-backoff available-at time; a retryable failure on the last
// attempt moves it to dead. Both failed and dead are terminal and inspectable
// via ListDead and Get; succeeded rows are retained the same way so that
// "did this run, and what happened" is always answerable by query.
// PruneTerminal deletes terminal rows older than a caller-chosen cutoff;
// nothing prunes automatically.
//
// QueueLag reports the age of the oldest pending record that is due now.
// Export it as the durable_work_queue_lag_seconds gauge; it is the one
// queue-health signal this package defines.
//
// # The external-effect rule, with one worked handler
//
// A handler that performs an external effect (anything outside the
// enqueue/claim transaction: an API call, a file write, a registry push)
// persists intent before the call and records the observed outcome after it.
// The package does not enforce this with a framework; each handler follows it
// by construction. The GitHub source-sync handler is the worked example:
//
//  1. Side-effect-free reads need no intent. GetBranchHead, GetCommitMetadata,
//     and FetchArchive are GitHub reads; retrying them after worker death is
//     safe by definition.
//  2. Content-addressed writes converge. StoreSourceArchive keys bytes by
//     digest, so a duplicate store after death writes the same bytes under
//     the same key instead of corrupting state.
//  3. The product mutation is the atomic commit point. ObserveSourceRevision,
//     UpsertSourceSnapshot, the build row, and the deployment transition
//     commit in one product transaction inside QueueSourceBuild. Death before
//     the commit leaves no product trace and the retry re-runs cleanly;
//     death after the commit leaves committed product state that the retry
//     must tolerate.
//  4. Completion is fenced. Complete runs after the commit as a CAS on
//     (owner, epoch), so only the owner that performed the work can mark it
//     succeeded. A stalled owner whose lease was taken over gets ErrLeaseLost
//     instead of double-committing.
//
// The handler is at-least-once across the commit boundary: death after the
// product commit but before Complete retries the handler, and the retry
// queues a second build for the same commit. Duplicate builds converge
// because queueing supersedes older queued builds for the service and both
// builds pin the same source digest. Handlers whose external effects are not
// naturally idempotent must persist an intent row in the pre-call transaction
// and check it before repeating the call; that check lives in the handler,
// not in this package.
package durablework
