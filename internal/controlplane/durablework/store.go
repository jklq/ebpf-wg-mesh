package durablework

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"ebof-wg-mesh/internal/controlplane/dbtx"
)

// Transaction runs fn inside a retried coordination transaction.
type Transaction func(context.Context, func(context.Context, *sql.Tx) error) error

// Store is a CockroachDB-backed durable work queue. EnqueueTx joins the
// caller's transaction; every other method manages its own.
type Store struct {
	db     *sql.DB
	withTx Transaction
	ready  chan struct{}
}

// NewStore builds a queue over db. withTx runs coordination transactions
// with serializable retries; it must not advance the product journal.
func NewStore(db *sql.DB, withTx Transaction) *Store {
	return &Store{db: db, withTx: withTx, ready: make(chan struct{}, 1)}
}

// Ready is closed-signalled whenever an enqueue makes work available. Like
// all wakeups it is best-effort: receivers must also poll.
func (s *Store) Ready() <-chan struct{} { return s.ready }

func (s *Store) signal() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

const recordColumns = `id, kind, dedup_key, resource_type, resource_id, state,
	attempt_count, attempt_limit, owner_id, owner_epoch, lease_expires_at,
	last_error, available_at, payload, created_at, updated_at, completed_at`

func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var rec Record
	err := row.Scan(&rec.ID, &rec.Kind, &rec.DedupKey, &rec.ResourceType, &rec.ResourceID,
		&rec.State, &rec.AttemptCount, &rec.AttemptLimit, &rec.OwnerID, &rec.OwnerEpoch,
		&rec.LeaseExpiresAt, &rec.LastError, &rec.AvailableAt, &rec.Payload,
		&rec.CreatedAt, &rec.UpdatedAt, &rec.CompletedAt)
	return rec, err
}

// Enqueue inserts params, deduplicating on the dedup key while a record is
// active and resurrecting it to pending when it is terminal. It reports
// whether the queue accepted the work. Prefer EnqueueTx: enqueue in the same
// transaction as the product-state mutation that requires the work.
func (s *Store) Enqueue(ctx context.Context, params EnqueueParams) (bool, error) {
	var err error
	if params, err = params.validated(); err != nil {
		return false, err
	}
	var enqueued bool
	err = s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		enqueued, err = s.EnqueueTx(ctx, tx, params)
		return err
	})
	return enqueued, err
}

// EnqueueTx is Enqueue joined to the caller's transaction.
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, params EnqueueParams) (bool, error) {
	params, err := params.validated()
	if err != nil {
		return false, err
	}
	now, err := dbtx.DatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	availableAt := params.AvailableAt
	if availableAt.IsZero() {
		availableAt = now
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO durable_work_items(
			id, kind, dedup_key, resource_type, resource_id, state,
			attempt_count, attempt_limit, owner_id, owner_epoch,
			lease_expires_at, last_error, available_at, payload,
			created_at, updated_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, 0, $7, '', 0, NULL, '', $8, $9, $10, $10, NULL)
		ON CONFLICT(dedup_key) DO UPDATE SET
			kind = excluded.kind,
			resource_type = excluded.resource_type,
			resource_id = excluded.resource_id,
			state = $6,
			attempt_count = 0,
			attempt_limit = excluded.attempt_limit,
			owner_id = '',
			lease_expires_at = NULL,
			last_error = '',
			available_at = excluded.available_at,
			payload = excluded.payload,
			updated_at = excluded.updated_at,
			completed_at = NULL
		WHERE durable_work_items.state IN ($11, $12, $13)`,
		uuid.NewString(), params.Kind, params.DedupKey, params.ResourceType, params.ResourceID,
		StatePending, params.AttemptLimit, availableAt, params.Payload, now,
		StateSucceeded, StateFailed, StateDead,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	enqueued := rows > 0
	if enqueued {
		s.signal()
	}
	return enqueued, nil
}

// maxClaimSweeps bounds how many exhausted records one Claim dead-letters
// before either leasing a live record or reporting no work.
const maxClaimSweeps = 8

// Claim leases one due record to ownerID for leaseTTL. It considers pending
// records whose available-at time has passed and leased records whose lease
// has expired, oldest first, optionally restricted to kinds. Every claim
// increments the owner epoch and the attempt count; the guarded UPDATE is
// the compare-and-swap, so concurrent replicas converge on a single winner.
// Records that exhausted their attempt limit are moved to dead instead of
// leased. An empty Record with a nil error means no work is due.
func (s *Store) Claim(ctx context.Context, ownerID string, leaseTTL time.Duration, kinds ...string) (Record, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return Record{}, errors.New("durable work owner id is required")
	}
	if leaseTTL <= 0 {
		return Record{}, errors.New("durable work lease TTL must be positive")
	}
	kinds = cleanKinds(kinds)
	var claimed Record
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		claimed = Record{}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		for range maxClaimSweeps {
			rec, err := selectClaimCandidateTx(ctx, tx, now, kinds)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if rec.AttemptCount >= rec.AttemptLimit {
				moved, err := markDeadExhaustedTx(ctx, tx, now, rec)
				if err != nil {
					return err
				}
				if !moved {
					return nil
				}
				continue
			}
			leased, err := leaseRecordTx(ctx, tx, now, rec, ownerID, leaseTTL)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			claimed = leased
			return nil
		}
		return nil
	})
	return claimed, err
}

func cleanKinds(kinds []string) []string {
	var out []string
	for _, kind := range kinds {
		if trimmed := strings.TrimSpace(kind); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func selectClaimCandidateTx(ctx context.Context, tx *sql.Tx, now time.Time, kinds []string) (Record, error) {
	var query strings.Builder
	args := []any{now}
	query.WriteString(`SELECT ` + recordColumns + ` FROM durable_work_items
		WHERE ((state = $2 AND available_at <= $1)
		    OR (state = $3 AND lease_expires_at IS NOT NULL AND lease_expires_at <= $1))`)
	args = append(args, StatePending, StateLeased)
	if len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			args = append(args, kind)
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
		query.WriteString(` AND kind IN (` + strings.Join(placeholders, ", ") + `)`)
	}
	query.WriteString(` ORDER BY available_at ASC, created_at ASC, id ASC LIMIT 1 FOR UPDATE`)
	return scanRecord(tx.QueryRowContext(ctx, query.String(), args...))
}

// markDeadExhaustedTx moves an attempt-exhausted record to dead. The UPDATE
// is guarded on the observed (state, owner, epoch): a concurrent claim that
// won the row first makes this a no-op reported as moved=false.
func markDeadExhaustedTx(ctx context.Context, tx *sql.Tx, now time.Time, rec Record) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE durable_work_items
		    SET state = $1,
		        lease_expires_at = NULL,
		        last_error = CASE WHEN last_error = '' THEN $2 ELSE last_error END,
		        completed_at = $3,
		        updated_at = $3
		  WHERE id = $4 AND state = $5 AND owner_id = $6 AND owner_epoch = $7`,
		StateDead, "attempt limit exhausted", now,
		rec.ID, rec.State, rec.OwnerID, rec.OwnerEpoch,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// leaseRecordTx claims the candidate row. CockroachDB does not support SKIP
// LOCKED; the SELECT FOR UPDATE row lock plus this guarded UPDATE preserves
// a single winner under serializable retries. sql.ErrNoRows reports a lost
// race.
func leaseRecordTx(ctx context.Context, tx *sql.Tx, now time.Time, rec Record, ownerID string, leaseTTL time.Duration) (Record, error) {
	expiresAt := now.Add(leaseTTL)
	return scanRecord(tx.QueryRowContext(ctx,
		`UPDATE durable_work_items AS work
		    SET state = $1,
		        owner_id = $2,
		        owner_epoch = work.owner_epoch + 1,
		        attempt_count = work.attempt_count + 1,
		        lease_expires_at = $3,
		        updated_at = $4
		  WHERE work.id = $5
		    AND work.state = $6
		    AND work.owner_id = $7
		    AND work.owner_epoch = $8
		    AND (work.state = $9 OR work.lease_expires_at <= $4)
		RETURNING `+returningColumns("work"),
		StateLeased, ownerID, expiresAt, now,
		rec.ID, rec.State, rec.OwnerID, rec.OwnerEpoch, StatePending,
	))
}

func returningColumns(alias string) string {
	parts := strings.Split(recordColumns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// Heartbeat extends rec's lease by leaseTTL. It is a single CAS on the
// claimed (owner, epoch): a superseded owner gets ErrLeaseLost.
func (s *Store) Heartbeat(ctx context.Context, rec Record, leaseTTL time.Duration) error {
	if rec.ID == "" || rec.OwnerID == "" {
		return errors.New("durable work heartbeat requires a claimed record")
	}
	if leaseTTL <= 0 {
		return errors.New("durable work lease TTL must be positive")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE durable_work_items
		    SET lease_expires_at = statement_timestamp() + $1::INT8 * INTERVAL '1 microsecond',
		        updated_at = statement_timestamp()
		  WHERE id = $2 AND state = $3 AND owner_id = $4 AND owner_epoch = $5`,
		leaseTTL.Microseconds(), rec.ID, StateLeased, rec.OwnerID, rec.OwnerEpoch,
	)
	if err != nil {
		return err
	}
	return requireLeaseRow(result, rec)
}

// Complete marks rec succeeded. It is a single CAS on the claimed (owner,
// epoch): a superseded owner gets ErrLeaseLost and the record stays with
// its current owner.
func (s *Store) Complete(ctx context.Context, rec Record) error {
	if rec.ID == "" || rec.OwnerID == "" {
		return errors.New("durable work complete requires a claimed record")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE durable_work_items
		    SET state = $1,
		        lease_expires_at = NULL,
		        completed_at = statement_timestamp(),
		        updated_at = statement_timestamp()
		  WHERE id = $2 AND state = $3 AND owner_id = $4 AND owner_epoch = $5`,
		StateSucceeded, rec.ID, StateLeased, rec.OwnerID, rec.OwnerEpoch,
	)
	if err != nil {
		return err
	}
	return requireLeaseRow(result, rec)
}

// Fail dispositions a leased record after a failed attempt. Non-retryable
// failures move it to failed at once; retryable failures with attempts
// remaining move it back to pending with a jittered backoff available-at
// time, and retryable failures on the last attempt move it to dead. The
// whole transition is one CAS on the claimed (owner, epoch): a superseded
// owner gets ErrLeaseLost.
func (s *Store) Fail(ctx context.Context, rec Record, processErr error, opts FailOptions) error {
	if rec.ID == "" || rec.OwnerID == "" {
		return errors.New("durable work fail requires a claimed record")
	}
	delay := opts.RetryAfter
	if delay <= 0 {
		base, max := opts.bounds()
		delay = RetryDelay(rec.AttemptCount, base, max)
	} else {
		delay = jitter(delay)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE durable_work_items
		    SET state = CASE
		          WHEN NOT $5 THEN $6
		          WHEN attempt_count >= attempt_limit THEN $7
		          ELSE $8
		        END,
		        owner_id = CASE
		          WHEN $5 AND attempt_count < attempt_limit THEN ''
		          ELSE owner_id
		        END,
		        lease_expires_at = NULL,
		        last_error = $9,
		        available_at = CASE
		          WHEN $5 AND attempt_count < attempt_limit
		          THEN statement_timestamp() + $10::INT8 * INTERVAL '1 microsecond'
		          ELSE available_at
		        END,
		        completed_at = CASE
		          WHEN $5 AND attempt_count < attempt_limit THEN NULL
		          ELSE statement_timestamp()
		        END,
		        updated_at = statement_timestamp()
		  WHERE id = $1 AND state = $2 AND owner_id = $3 AND owner_epoch = $4`,
		rec.ID, StateLeased, rec.OwnerID, rec.OwnerEpoch,
		opts.Retryable, StateFailed, StateDead, StatePending,
		SanitizeError(processErr), delay.Microseconds(),
	)
	if err != nil {
		return err
	}
	return requireLeaseRow(result, rec)
}

func requireLeaseRow(result sql.Result, rec Record) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return leaseLost(rec.ID, rec.OwnerID, rec.OwnerEpoch)
	}
	return nil
}

// Get loads one record by ID for inspection.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	return scanRecord(s.db.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM durable_work_items WHERE id = $1`, id))
}

// ListDead returns dead records, newest first, optionally restricted to one
// kind. Limit <= 0 means a bounded default.
func (s *Store) ListDead(ctx context.Context, kind string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT ` + recordColumns + ` FROM durable_work_items WHERE state = $1`
	args := []any{StateDead}
	if trimmed := strings.TrimSpace(kind); trimmed != "" {
		query += ` AND kind = $2`
		args = append(args, trimmed)
	}
	query += ` ORDER BY updated_at DESC, id ASC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// QueueLag reports the age of the oldest pending record that is due now,
// optionally restricted to kinds. Zero means no record is waiting. Export
// this value as the durable_work_queue_lag_seconds gauge.
func (s *Store) QueueLag(ctx context.Context, kinds ...string) (time.Duration, error) {
	kinds = cleanKinds(kinds)
	query := `SELECT COALESCE(EXTRACT(EPOCH FROM (statement_timestamp() - MIN(available_at))), 0)
		FROM durable_work_items
		WHERE state = $1 AND available_at <= statement_timestamp()`
	args := []any{StatePending}
	if len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			args = append(args, kind)
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
		query += ` AND kind IN (` + strings.Join(placeholders, ", ") + `)`
	}
	var seconds float64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&seconds); err != nil {
		return 0, err
	}
	if seconds < 0 {
		return 0, nil
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// PruneTerminal deletes terminal records completed before cutoff, up to
// limit rows, and reports how many it removed. Nothing prunes
// automatically; operators and future retention loops call this explicitly.
// Limit <= 0 means a bounded default.
func (s *Store) PruneTerminal(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM durable_work_items
		  WHERE id IN (
			SELECT id FROM durable_work_items
			 WHERE state IN ($1, $2, $3)
			   AND completed_at IS NOT NULL
			   AND completed_at < $4
			 ORDER BY completed_at ASC, id ASC
			 LIMIT $5
		  )`,
		StateSucceeded, StateFailed, StateDead, cutoff.UTC(), limit,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
