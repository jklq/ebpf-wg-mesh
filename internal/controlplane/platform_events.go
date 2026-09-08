package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	maxPlatformBlockingWait    = 5 * time.Minute
	defaultEventPollInterval   = 250 * time.Millisecond
	initialEnvironmentRevision = int64(1)
	globalEnvironmentEventID   = "__control_plane_global__"
)

// PlatformEvents provides durable monotonic indexes and blocking waits. The
// revision is intentionally global: false-positive wakes are cheap, while
// advancing it in every database.withTx transaction makes state and notification
// atomic without requiring callers to remember an environment-specific outbox.
type PlatformEvents struct {
	store        environmentEventStore
	pollInterval time.Duration
}

type environmentEventStore interface {
	publishEnvironmentEvent(context.Context, string) (int64, error)
	currentEnvironmentEvent(context.Context, string) (int64, error)
}

func NewPlatformEvents(store environmentEventStore, pollInterval time.Duration) *PlatformEvents {
	if pollInterval <= 0 {
		pollInterval = defaultEventPollInterval
	}
	return &PlatformEvents{store: store, pollInterval: pollInterval}
}

func (s *eventsPersistence) publishEnvironmentEvent(ctx context.Context, environmentID string) (int64, error) {
	// Visible store mutations advance the global revision in database.withTx in the
	// same transaction as the state change. Publishing is therefore a durable
	// read, not a second write that could be lost after the state commits.
	return s.currentEnvironmentEvent(ctx, environmentID)
}

func (e *PlatformEvents) Current(ctx context.Context, environmentID string) (int64, error) {
	if e == nil || e.store == nil || environmentID == "" {
		return initialEnvironmentRevision, nil
	}
	return e.store.currentEnvironmentEvent(ctx, environmentID)
}

func (s *eventsPersistence) currentEnvironmentEvent(ctx context.Context, environmentID string) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT revision FROM environment_events WHERE environment_id = $1`, globalEnvironmentEventID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return initialEnvironmentRevision, nil
	}
	return revision, err
}

func bumpGlobalEnvironmentEventTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_events(environment_id, revision, updated_at)
		VALUES ($1, $2, statement_timestamp())
		ON CONFLICT(environment_id) DO UPDATE
		SET revision = environment_events.revision + 1, updated_at = statement_timestamp()`,
		globalEnvironmentEventID, initialEnvironmentRevision+1)
	return err
}

// Wait returns the current index and whether it advanced beyond after.
func (e *PlatformEvents) Wait(ctx context.Context, environmentID string, after int64, timeout time.Duration) (int64, bool, error) {
	current, err := e.Current(ctx, environmentID)
	if err != nil {
		return 0, false, err
	}
	if after <= 0 || current > after {
		return current, true, nil
	}
	if timeout <= 0 || timeout > maxPlatformBlockingWait {
		timeout = maxPlatformBlockingWait
	}
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	poll := time.NewTicker(e.pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return current, false, ctx.Err()
		case <-timeoutTimer.C:
			return current, false, nil
		case <-poll.C:
			current, err = e.Current(ctx, environmentID)
			if err != nil {
				return 0, false, err
			}
			if current > after {
				return current, true, nil
			}
		}
	}
}

func platformWaitDuration(seconds int32) time.Duration {
	if seconds <= 0 {
		return maxPlatformBlockingWait
	}
	return time.Duration(seconds) * time.Second
}

func (e *PlatformEvents) Publish(ctx context.Context, environmentID string) (int64, error) {
	if e == nil || e.store == nil || environmentID == "" {
		return 0, nil
	}
	return e.store.publishEnvironmentEvent(ctx, environmentID)
}
