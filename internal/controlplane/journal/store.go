package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
	"github.com/google/uuid"
)

type Fence func(context.Context, *sql.Tx) (int64, error)

type CommitError struct {
	CommandID string
	Committed bool
	Err       error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("journal command %s (committed=%t): %v", e.CommandID, e.Committed, e.Err)
}
func (e *CommitError) Unwrap() error { return e.Err }

type headMovedError struct{}

func (headMovedError) Error() string    { return "journal head advanced during command" }
func (headMovedError) SQLState() string { return "40001" }

type Store struct {
	mu          sync.Mutex
	publishMu   sync.Mutex
	db          *sql.DB
	clusterID   string
	state       *Projection
	initialized bool
	fence       Fence
	onApplied   func(Applied)
	published   *Projection
	verify      bool
}

func (s *Store) SetVerifyRecordings(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verify = enabled
}

func New(db *sql.DB, clusterID string, fence Fence) *Store {
	return &Store{db: db, clusterID: clusterID, state: newProjection(DurableState{ClusterID: clusterID}), fence: fence}
}

// SetOnApplied registers a listener invoked after the in-memory prefix advances.
// The callback receives the shared immutable projection and exact committed
// changes. Unchanged reads do not publish. It must not re-enter the journal.
type Applied struct {
	Projection *Projection
	Batches    []Batch
	Reset      bool
}

func (s *Store) SetOnApplied(fn func(Applied)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onApplied = fn
}

type commandIDKey struct{}

func WithCommandID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, commandIDKey{}, id)
}

func (s *Store) Execute(ctx context.Context, fn func(context.Context, *sql.Tx) error) (Entry, error) {
	return s.execute(ctx, fn, nil)
}

// ExecuteWithMutation runs one journal command, then lets afterChanges react to
// the exact rows changed. afterChanges runs in the same transaction before
// serialization; rows it changes through Mutation join the command payload.
//
// fn must contain only transactional work and capture every durable row it
// changes. Planning happens outside the heads lock; the command retries on
// serialization conflicts and publishes no attempted state.
func (s *Store) ExecuteWithMutation(ctx context.Context, fn func(context.Context, *sql.Tx) error, afterChanges func(context.Context, *sql.Tx, *Projection, Batch) error) (Entry, error) {
	return s.execute(ctx, fn, afterChanges)
}

func (s *Store) authorizeScheduling(ctx context.Context, tx *sql.Tx, batch Batch) (*int64, error) {
	if !batch.requiresAuthority() {
		return nil, nil
	}
	if s.fence == nil {
		return nil, errors.New("scheduling decision requires authority")
	}
	epoch, err := s.fence(ctx, tx)
	if err != nil {
		return nil, err
	}
	if epoch <= 0 {
		return nil, errors.New("invalid scheduling authority epoch")
	}
	return &epoch, nil
}

func (s *Store) execute(ctx context.Context, fn func(context.Context, *sql.Tx) error, afterChanges func(context.Context, *sql.Tx, *Projection, Batch) error) (Entry, error) {
	s.mu.Lock()
	verify := s.verify
	s.mu.Unlock()
	id, _ := ctx.Value(commandIDKey{}).(string)
	callerProvidedID := id != ""
	if id == "" {
		id = uuid.NewString()
	}
	receiptLifetime := InternalReceiptLifetime
	if callerProvidedID {
		receiptLifetime = CallerProvidedReceiptLifetime
	}
	var receipt Entry
	appended := false
	recordedNoOp := false
	resolvedReceipt := false
	resolvedNoOp := false
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		appended = false
		recordedNoOp = false
		resolvedReceipt = false
		resolvedNoOp = false
		receipt = Entry{}
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1`, s.clusterID).Scan(&head); err != nil {
			return err
		}
		var err error
		receipt, err = lookup(ctx, tx, s.clusterID, id)
		if err == nil {
			resolvedReceipt = true
			var batch Batch
			resolvedNoOp = json.Unmarshal(receipt.Payload, &batch) == nil && batch.Empty()
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		base, _, err := s.stateAtHead(ctx, tx, head)
		if err != nil {
			return err
		}
		recorder := newRecorder(tx)
		commandCtx := withRecorder(ctx, recorder)
		if err := fn(commandCtx, tx); err != nil {
			return err
		}
		batch, err := resolveRecorded(ctx, tx, base.DurableState, recorder.keys)
		if err != nil {
			return err
		}
		if afterChanges != nil {
			if err := afterChanges(commandCtx, tx, base, batch); err != nil {
				return err
			}
			batch, err = resolveRecorded(ctx, tx, base.DurableState, recorder.keys)
			if err != nil {
				return err
			}
		}
		if verify {
			if err := verifyRecordings(ctx, tx, base.DurableState, batch); err != nil {
				return err
			}
		}
		if batch.Empty() {
			if !callerProvidedID {
				return nil
			}
			receipt = Entry{ClusterID: s.clusterID, LogIndex: head, CommandID: id, CommandVersion: CommandVersion, CommandType: CommandType}
			receipt.Payload, err = json.Marshal(batch)
			if err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO cluster_journal_receipts
				(cluster_id, command_id, log_index, command_version, command_type, payload, authorizing_epoch, created_at, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, NULL, statement_timestamp(), statement_timestamp() + $7::INT8 * INTERVAL '1 microsecond') RETURNING created_at`,
				receipt.ClusterID, receipt.CommandID, receipt.LogIndex, receipt.CommandVersion, receipt.CommandType, receipt.Payload, receiptLifetime.Microseconds()).Scan(&receipt.CreatedAt); err != nil {
				return err
			}
			recordedNoOp = true
			return nil
		}
		receipt = Entry{ClusterID: s.clusterID, LogIndex: head + 1, CommandID: id, CommandVersion: CommandVersion, CommandType: CommandType}
		epoch, err := s.authorizeScheduling(ctx, tx, batch)
		if err != nil {
			return err
		}
		receipt.AuthorizingEpoch = epoch
		receipt.Payload, err = json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, _, err := base.advance(receipt); err != nil {
			return err
		}
		var lockedHead int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1 FOR UPDATE`, s.clusterID).Scan(&lockedHead); err != nil {
			return err
		}
		if lockedHead != head {
			return headMovedError{}
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO cluster_journal
   (cluster_id, log_index, command_id, command_version, command_type, payload, authorizing_epoch, created_at)
   VALUES ($1, $2, $3, $4, $5, $6, $7, statement_timestamp()) RETURNING created_at`,
			receipt.ClusterID, receipt.LogIndex, receipt.CommandID, receipt.CommandVersion, receipt.CommandType, receipt.Payload, receipt.AuthorizingEpoch).Scan(&receipt.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cluster_journal_receipts
			(cluster_id, command_id, log_index, command_version, command_type, payload, authorizing_epoch, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8::TIMESTAMPTZ + $9::INT8 * INTERVAL '1 microsecond')`,
			receipt.ClusterID, receipt.CommandID, receipt.LogIndex, receipt.CommandVersion, receipt.CommandType, receipt.Payload, receipt.AuthorizingEpoch, receipt.CreatedAt, receiptLifetime.Microseconds()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE cluster_journal_heads SET log_index = $2 WHERE cluster_id = $1`, s.clusterID, receipt.LogIndex)
		appended = err == nil
		return err
	})
	if err == nil && !appended {
		if recordedNoOp || resolvedNoOp {
			return receipt, nil
		}
		if !resolvedReceipt {
			return Entry{}, nil
		}
	}
	// A failed COMMIT may have succeeded. Resolve against the same command ID on
	// a fresh context; absence/error never permits publish.
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	committed, resolveErr := lookup(resolveCtx, s.db, s.clusterID, id)
	if resolveErr != nil {
		if err != nil {
			return Entry{}, &CommitError{CommandID: id, Err: err}
		}
		return Entry{}, &CommitError{CommandID: id, Committed: true, Err: resolveErr}
	}
	catchErr := s.Read(resolveCtx, nil)
	if catchErr != nil {
		return committed, &CommitError{CommandID: id, Committed: true, Err: catchErr}
	}
	return committed, nil
}

func (s *Store) Receipt(ctx context.Context, commandID string) (Entry, error) {
	return lookup(ctx, s.db, s.clusterID, commandID)
}

func (s *Store) Snapshot(ctx context.Context) (DurableState, error) {
	var snapshot DurableState
	err := s.Read(ctx, func(_ *sql.Tx, p *Projection) error {
		snapshot = p.DurableState.Clone()
		return nil
	})
	if err != nil {
		return DurableState{}, err
	}
	return snapshot, nil
}

// Read builds an external snapshot from the same database snapshot as the
// applied prefix. The result must not be published inside fn.
func (s *Store) Read(ctx context.Context, fn func(*sql.Tx, *Projection) error) error {
	return s.read(ctx, fn)
}

// publish orders callbacks while keeping SQL, replay, and readers outside the
// lock. A transaction may finish after a newer database snapshot was published.
func (s *Store) publish(next *Projection, batches []Batch) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.mu.Lock()
	reset := s.published == nil
	if s.initialized && next.LogIndex <= s.state.LogIndex {
		if s.onApplied == nil || !reset {
			s.mu.Unlock()
			return
		}
		next, batches = s.state, nil
	} else {
		for len(batches) > 0 && batches[0].BaseIndex < s.state.LogIndex {
			batches = batches[1:]
		}
		if len(batches) == 0 || batches[0].BaseIndex != s.state.LogIndex {
			reset = true
		}
		s.state, s.initialized = next, true
	}
	hook := s.onApplied
	if hook != nil {
		s.published = next
	}
	s.mu.Unlock()
	if hook != nil {
		hook(Applied{Projection: next, Batches: batches, Reset: reset})
	}
}

func (s *Store) read(ctx context.Context, fn func(*sql.Tx, *Projection) error) error {
	var next *Projection
	var batches []Batch
	err := sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1`, s.clusterID).Scan(&head); err != nil {
			return err
		}
		var err error
		next, batches, err = s.stateAtHead(ctx, tx, head)
		if err != nil {
			return err
		}
		if fn != nil {
			return fn(tx, next)
		}
		return nil
	})
	if err == nil {
		s.publish(next, batches)
	}
	return err
}

func (s *Store) stateAtHead(ctx context.Context, tx *sql.Tx, head int64) (*Projection, []Batch, error) {
	var compacted int64
	if err := tx.QueryRowContext(ctx, `SELECT compacted_index FROM cluster_journal_heads WHERE cluster_id = $1`, s.clusterID).Scan(&compacted); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	state, initialized := s.state, s.initialized
	s.mu.Unlock()
	if !initialized || state.LogIndex < compacted || state.LogIndex > head {
		state, err := readProductState(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		state.ClusterID = s.clusterID
		state.LogIndex = head
		return newProjection(state), nil, nil
	}
	return replay(ctx, tx, state, head)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const entryColumns = `cluster_id, log_index, command_id, command_version, command_type, payload, authorizing_epoch, created_at`

type scanner interface{ Scan(...any) error }

func scanEntry(row scanner) (Entry, error) {
	var entry Entry
	err := row.Scan(&entry.ClusterID, &entry.LogIndex, &entry.CommandID, &entry.CommandVersion, &entry.CommandType, &entry.Payload, &entry.AuthorizingEpoch, &entry.CreatedAt)
	return entry, err
}

func lookup(ctx context.Context, q queryer, clusterID, commandID string) (Entry, error) {
	return scanEntry(q.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM cluster_journal_receipts WHERE cluster_id = $1 AND command_id = $2`, clusterID, commandID))
}

func verifyRecordings(ctx context.Context, tx *sql.Tx, base DurableState, recorded Batch) error {
	after, err := readProductState(ctx, tx)
	if err != nil {
		return err
	}
	recordedJSON, err := json.Marshal(recorded)
	if err != nil {
		return err
	}
	actualJSON, err := json.Marshal(Diff(base, after))
	if err != nil {
		return err
	}
	if !bytes.Equal(recordedJSON, actualJSON) {
		return fmt.Errorf("journal recording mismatch\nrecorded: %s\nactual:   %s", recordedJSON, actualJSON)
	}
	return nil
}

func replay(ctx context.Context, tx *sql.Tx, state *Projection, head int64) (*Projection, []Batch, error) {
	var batches []Batch
	if head == state.LogIndex {
		return state, nil, nil
	}
	if head < state.LogIndex {
		return state, nil, errors.New("journal head moved backwards")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+entryColumns+` FROM cluster_journal WHERE cluster_id = $1 AND log_index > $2 AND log_index <= $3 ORDER BY log_index`, state.ClusterID, state.LogIndex, head)
	if err != nil {
		return state, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return state, nil, err
		}
		var batch Batch
		state, batch, err = state.advance(entry)
		if err != nil {
			return state, nil, err
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return state, nil, err
	}
	if state.LogIndex != head {
		return state, nil, fmt.Errorf("journal gap: applied %d, head %d", state.LogIndex, head)
	}
	return state, batches, nil
}
