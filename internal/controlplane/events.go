package controlplane

import (
	"context"
	"database/sql"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
)

const (
	maxPlatformBlockingWait    = 5 * time.Minute
	defaultEventPollInterval   = 250 * time.Millisecond
	initialEnvironmentRevision = dbtx.InitialEnvironmentRevision
	globalEnvironmentEventID   = dbtx.GlobalEnvironmentEventID
)

type platformEvents struct {
	store        globalRevisionStore
	pollInterval time.Duration
}

type globalRevisionStore interface {
	currentGlobalRevision(context.Context) (int64, error)
}

func newPlatformEvents(store globalRevisionStore, pollInterval time.Duration) *platformEvents {
	if pollInterval <= 0 {
		pollInterval = defaultEventPollInterval
	}
	return &platformEvents{store: store, pollInterval: pollInterval}
}

func (e *platformEvents) Current(ctx context.Context) (int64, error) {
	if e == nil || e.store == nil {
		return initialEnvironmentRevision, nil
	}
	return e.store.currentGlobalRevision(ctx)
}

func (s *database) currentGlobalRevision(ctx context.Context) (int64, error) {
	return dbtx.EnvironmentRevision(ctx, s.db)
}

func bumpGlobalEnvironmentEventTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_events(environment_id, revision, updated_at)
		VALUES ($1, $2, statement_timestamp())
		ON CONFLICT(environment_id) DO UPDATE
		SET revision = environment_events.revision + 1, updated_at = statement_timestamp()`,
		globalEnvironmentEventID, initialEnvironmentRevision+1)
	return err
}

func (e *platformEvents) Wait(ctx context.Context, after int64, timeout time.Duration) (int64, bool, error) {
	current, err := e.Current(ctx)
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
			current, err = e.Current(ctx)
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

func (s *database) bumpAffectedAgents(ctx context.Context, tx *sql.Tx, base *journal.Projection, batch journal.Batch) error {
	after, err := base.Preview(batch)
	if err != nil {
		return err
	}
	return dbtx.BumpDesiredRevisions(ctx, tx, deliverycore.AffectedAgentIDs(base, after, batch))
}
