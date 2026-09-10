# Authoritative agent reconciliation

The stream has one delivery mode: complete desired snapshots. There is no diff
history, command replay, or resume protocol. Reconnecting sends a fresh hello and
receives a complete snapshot even if the accepted cursor has not changed.

## Identity and sessions

mTLS authenticates the agent ID and the cluster trust root. The cluster ID is the
SHA-256 fingerprint of the enrolled CA PEM (with surrounding whitespace removed).
Hello carries both identities, a durable local-store UUID, initialization state
(`uninitialized`, `ready`, or `recovery`), a random stream ID, a durable increasing
session incarnation, the last accepted epoch/cursor, allocation desired/applied
generations and runtime phases, and independently discovered runtime identities.

The server binds an enrollment to its first local-store UUID. Another store
cannot reuse that enrollment, including an empty store with valid credentials.
The server accepts an incarnation only if it exceeds the persisted incarnation.
A delayed hello therefore cannot replace a newer session. Reports, heartbeats,
acknowledgements and command grants are scoped to the current stream ID.

An identity mismatch fails explicitly. The local cluster binding is never
rewritten on reconnect; a local cluster mismatch persists identity recovery and
blocks further acceptance, including after restart. A fresh store rejected by the
server does not receive desired state. Recovery requires accounting for surviving
workloads, retiring/revoking the old identity, and enrolling a replacement identity
with separate storage. Copying or clearing identity metadata is not recovery.
Automatic trust-root rotation or identity rebinding is not supported by this
protocol.

## Snapshot acceptance and removal

The server reads the cursor, epoch, node/network configuration, identities,
volumes and allocations in one database transaction. Only a successful read
produces `complete = true`, with scope `AGENT`. The authenticated cluster ID and
agent ID identify the scope; a snapshot cannot authorize cleanup on another node.
Every delivered snapshot also names its stream and carries an authority expiry.

The agent validates identity, stream, scope, completeness, configuration, expiry,
epoch and cursor. It persists the highest observed authenticated epoch separately
from the last accepted epoch/cursor: learning a newer epoch from a rejected
snapshot still fences older authority after restart. Within one epoch, cursors
must not decrease; different configuration at the same cursor is rejected.
Duplicate configuration is accepted idempotently. Credentials and transport
metadata may be renewed without inventing a new configuration cursor.

Acceptance uses two local transactions. First the complete candidate, including
its cursor and credentials, is durably staged. Staging changes neither the
accepted position nor any runtime-visible configuration. Then the agent prepares
the accepted snapshot, cursor, credentials and allocation operation records in a
second transaction and checks the grant again as its final acceptance decision.
This check is the acceptance **linearization point**: the exact candidate is
already durable, and authority must still be valid. A persistence stall or
process pause before that check cannot admit an expired candidate.

The second transaction atomically publishes that decision. Its commit and the
acknowledgement may finish after expiry, just as runtime work for an already
accepted snapshot may finish later; they do not make a new authority decision.
There is no bound assumed on disk latency. Runtime reconciliation cannot see the
candidate until publication commits, and acknowledgement follows that commit.
A failed write or expired decision sends no acknowledgement and retains the
previous accepted snapshot and cursor. Startup discards any candidate without a
committed decision, even if its grant is still valid. If publication commits but
its acknowledgement is lost, reconnect and a duplicate snapshot recover without
replaying commands. Acceptance is serialized with runtime reconciliation so an
older reconcile cannot start work after a newer removal has been accepted.

Omission requests removal only in an accepted complete agent scope. Missing,
partial, invalid, expired or stale snapshots cannot clear existing desired state.
Unowned resources discovered during recovery remain protected until ownership is
established. Runtime progress, health and release are observations, never implied
by an acceptance acknowledgement. Acknowledgements update acceptance position;
they do not advance observation sequence or applied generations.

## Authority expiration and takeover

Authority is the shared database, rather than an individual streaming replica.
For each snapshot the sender obtains a **15-second grant** from database time,
checking both the epoch and current reachable session transactionally. The
maximum outstanding expiry is persisted in `agent_authority`. A sender that
pauses before transmitting can deliver only that original deadline; it cannot
extend the grant locally. A disconnected sender cannot mint grants without the
database. A replica reading a different epoch must establish a new session.

**Clock assumption:** agent wall time must remain within **one second** of database
time, including after suspend/resume. The agent admits a command only when
`agent_time + 1 second < authority_not_after`, and rejects deadlines beyond the
maximum grant lifetime plus that skew allowance. Clock synchronization is an
operational prerequisite; this code does not detect arbitrary clock failure.
These guarantees concern crash, delay and partition failures, not a malicious
control-plane replica fabricating grants over its authenticated connection.

Takeover must stop old-epoch grant issuance and wait until database time reaches
the persisted maximum expiry. `advanceAgentAuthority` checks the expected epoch
and that deadline in a serializable transaction before incrementing the epoch.
Disconnect, process death, and early lease release never shorten the deadline.
Concurrent grant issuance either extends the wait or loses to the epoch change.
No automatic leaseholder takeover is enabled here; any future takeover caller
must use this gate and satisfy the clock assumption.

At the takeover instant, even the slowest permitted agent clock rejects the old
grant. An isolated agent therefore rejects a delayed old command without first
learning the new epoch. Grant expiry fences **new acceptance decisions over
durably staged snapshots**, not completion of previously made decisions. The
supervisor continues already accepted work during partitions and after restart;
expiry is not evidence that an allocation stopped or an exclusive writer released
storage. Workload replacement policy remains a separate prerequisite.

## Verification

`internal/agent/reconciliation_test.go` covers removal under duplicates, delayed
snapshots, stale sessions, reconnects, scope rejection, epoch persistence, sticky
identity recovery and a failure after transaction writes have begun. It also
advances the clock across takeover between staging and the acceptance decision,
checks that neither removal nor resurrection changes the accepted snapshot,
credentials, cursor or allocation operations, and restarts with an unaccepted
candidate to verify that it cannot become runtime-visible.
`internal/reconciliation/grant_test.go` checks isolated-agent expiry at both clock
skew extremes and takeover boundaries. The database integration tests in
`internal/controlplane/agent_authority_integration_test.go` wait through a real
grant deadline, test paused former senders, and reject delayed hellos, stale
acknowledgements and fresh-store identity reuse. Live TLS and control-plane
restart tests exercise both sides of the stream.

This is a clean schema/protocol cutover: control-plane schema 13 and local-store
format 2. Older persisted formats are refused; there is no compatibility mode.
