package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/google/uuid"
)

// Fence must check current decision authority in tx and hold it until commit.
// A nil fence cannot authorize a scheduling decision.
type Fence func(context.Context, *sql.Tx) (int64, error)

// CommitError preserves the command ID when a caller must resolve an outcome
// or resume application. A retry must carry this ID, not allocate a new one.
type CommitError struct {
	CommandID string
	Committed bool
	Err       error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("journal command %s (committed=%t): %v", e.CommandID, e.Committed, e.Err)
}
func (e *CommitError) Unwrap() error { return e.Err }

type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	clusterID string
	state     DurableState
	fence     Fence
}

func New(db *sql.DB, clusterID string, fence Fence) *Store {
	return &Store{db: db, clusterID: clusterID, state: DurableState{ClusterID: clusterID}, fence: fence}
}

type commandIDKey struct{}

// WithCommandID lets an operation carry its identity across request retries.
// Reusing an ID means returning its committed receipt without rerunning writes.
func WithCommandID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, commandIDKey{}, id)
}

// Execute serializes planning with journal application, including across
// replicas via the transactional head. fn must contain only transactional work.
// It is retried on serialization conflicts; no attempted state is published.
func (s *Store) Execute(ctx context.Context, fn func(*sql.Tx) error) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := ctx.Value(commandIDKey{}).(string)
	if id == "" {
		id = uuid.NewString()
	}
	var receipt Entry
	err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1 FOR UPDATE`, s.clusterID).Scan(&head); err != nil {
			return err
		}
		var err error
		receipt, err = lookup(ctx, tx, s.clusterID, id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		base, err := replay(ctx, tx, s.state, head)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		after, err := readProductState(ctx, tx)
		if err != nil {
			return err
		}
		batch := Diff(base, after)
		receipt = Entry{ClusterID: s.clusterID, LogIndex: head + 1, CommandID: id, CommandVersion: CommandVersion, CommandType: CommandType}
		// Assignment and rollout changes include reservations, allocation intent,
		// deadlines, and deployment progression. Staged product intent needs no epoch.
		if batch.requiresAuthority() {
			if s.fence == nil {
				return errors.New("scheduling decision requires authority")
			}
			epoch, err := s.fence(ctx, tx)
			if err != nil {
				return err
			}
			if epoch <= 0 {
				return errors.New("invalid scheduling authority epoch")
			}
			receipt.AuthorizingEpoch = &epoch
		}
		receipt.Payload, err = json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, err := base.Apply(receipt); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO cluster_journal
   (cluster_id, log_index, command_id, command_version, command_type, payload, authorizing_epoch, created_at)
   VALUES ($1, $2, $3, $4, $5, $6, $7, statement_timestamp()) RETURNING created_at`,
			receipt.ClusterID, receipt.LogIndex, receipt.CommandID, receipt.CommandVersion, receipt.CommandType, receipt.Payload, receipt.AuthorizingEpoch).Scan(&receipt.CreatedAt); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE cluster_journal_heads SET log_index = $2 WHERE cluster_id = $1`, s.clusterID, receipt.LogIndex)
		return err
	})
	// Even an apparently failed COMMIT may have succeeded. Resolve against the
	// same command ID using a fresh context; absence/error never permits publish.
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	committed, resolveErr := lookup(resolveCtx, s.db, s.clusterID, id)
	if resolveErr != nil {
		if err != nil {
			return Entry{}, &CommitError{CommandID: id, Err: err}
		}
		return Entry{}, &CommitError{CommandID: id, Committed: true, Err: resolveErr}
	}
	if err := s.catchUp(resolveCtx); err != nil {
		return committed, &CommitError{CommandID: id, Committed: true, Err: err}
	}
	return committed, nil
}

func (s *Store) Receipt(ctx context.Context, commandID string) (Entry, error) {
	return lookup(ctx, s.db, s.clusterID, commandID)
}

// Snapshot catches up to a committed head and returns an owned copy. Readers
// cannot mutate the state used to validate subsequent decisions.
func (s *Store) Snapshot(ctx context.Context) (DurableState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.catchUp(ctx); err != nil {
		return DurableState{}, err
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		return DurableState{}, err
	}
	var copy DurableState
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

func (s *Store) catchUp(ctx context.Context) error {
	return s.read(ctx, nil)
}

// Read builds an external snapshot from the same database snapshot as the
// applied journal prefix. The result must not be published inside fn.
func (s *Store) Read(ctx context.Context, fn func(*sql.Tx, DurableState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(ctx, fn)
}

func (s *Store) read(ctx context.Context, fn func(*sql.Tx, DurableState) error) error {
	var next DurableState
	err := crdb.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var head int64
		if err := tx.QueryRowContext(ctx, `SELECT log_index FROM cluster_journal_heads WHERE cluster_id = $1`, s.clusterID).Scan(&head); err != nil {
			return err
		}
		var err error
		next, err = replay(ctx, tx, s.state, head)
		if err != nil {
			return err
		}
		if fn != nil {
			return fn(tx, next)
		}
		return nil
	})
	if err == nil {
		s.state = next
	}
	return err
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
	return scanEntry(q.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM cluster_journal WHERE cluster_id = $1 AND command_id = $2`, clusterID, commandID))
}

func replay(ctx context.Context, tx *sql.Tx, state DurableState, head int64) (DurableState, error) {
	if head < state.LogIndex {
		return state, errors.New("journal head moved backwards")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+entryColumns+` FROM cluster_journal WHERE cluster_id = $1 AND log_index > $2 AND log_index <= $3 ORDER BY log_index`, state.ClusterID, state.LogIndex, head)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return state, err
		}
		state, err = state.Apply(entry)
		if err != nil {
			return state, err
		}
	}
	if err := rows.Err(); err != nil {
		return state, err
	}
	if state.LogIndex != head {
		return state, fmt.Errorf("journal gap: applied %d, head %d", state.LogIndex, head)
	}
	return state, nil
}
