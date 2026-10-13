package controlplane

import (
	"context"
	"database/sql"
	"sync"
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
	mu           sync.Mutex
	waiters      map[chan struct{}]struct{}
	stopPolling  context.CancelFunc
	index        int64
	pollError    error
}

type globalRevisionStore interface {
	currentGlobalRevision(context.Context) (int64, error)
}

func newPlatformEvents(store globalRevisionStore, pollInterval time.Duration) *platformEvents {
	if pollInterval <= 0 {
		pollInterval = defaultEventPollInterval
	}
	return &platformEvents{store: store, pollInterval: pollInterval, waiters: make(map[chan struct{}]struct{})}
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
	updates, unsubscribe := e.subscribe()
	defer unsubscribe()
	// Register before rechecking so a commit between the initial read and
	// subscription cannot leave this caller waiting for the next commit.
	current, err = e.Current(ctx)
	if err != nil {
		return 0, false, err
	}
	if current > after {
		return current, true, nil
	}
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return current, false, ctx.Err()
		case <-timeoutTimer.C:
			return current, false, nil
		case <-updates:
			e.mu.Lock()
			current, err = e.index, e.pollError
			e.mu.Unlock()
			if err != nil {
				return 0, false, err
			}
			if current > after {
				return current, true, nil
			}
		}
	}
}

// All blocking readers on this replica share one database revision poll.
// The poller exists only while there are subscribers and still sees commits
// made by other replicas, which an in-process wakeup alone would miss.
func (e *platformEvents) subscribe() (<-chan struct{}, func()) {
	e.mu.Lock()
	updates := make(chan struct{}, 1)
	e.waiters[updates] = struct{}{}
	if e.stopPolling == nil {
		ctx, stop := context.WithCancel(context.Background())
		e.stopPolling = stop
		go e.poll(ctx)
	}
	e.mu.Unlock()
	return updates, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.waiters, updates)
		if len(e.waiters) == 0 && e.stopPolling != nil {
			e.stopPolling()
			e.stopPolling = nil
		}
	}
}

func (e *platformEvents) poll(ctx context.Context) {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		index, err := e.Current(ctx)
		e.mu.Lock()
		if ctx.Err() != nil {
			e.mu.Unlock()
			return
		}
		changed := index != e.index || err != nil || e.pollError != nil
		e.index, e.pollError = index, err
		if changed {
			for updates := range e.waiters {
				select {
				case updates <- struct{}{}:
				default:
				}
			}
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
