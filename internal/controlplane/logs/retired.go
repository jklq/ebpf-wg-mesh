package logs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"ebof-wg-mesh/internal/logpipeline"
	bolt "go.etcd.io/bbolt"
)

// ReplayRetiredJournals adopts only journals whose OS ownership lock is free.
// It shares the normal decoding and acknowledgement protocol. A crash after
// writing the sink leaves the cursor uncommitted, so stable log IDs deduplicate
// a retry. Journals belonging to live generations are never opened or reset.
func ReplayRetiredJournals(ctx context.Context, store flushStore, own string, budget int64) error {
	if store == nil || !store.Enabled() {
		return nil
	}
	entries, err := os.ReadDir(filepath.Dir(own))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(filepath.Dir(own), entry.Name())
		if !entry.IsDir() || path == own {
			continue
		}
		if _, err = os.Stat(filepath.Join(path, logpipeline.SpoolFile)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		a, err := NewAsyncIngester(store, AsyncIngesterConfig{SpoolDir: path, QueueBytes: budget, TryOwnership: true})
		if errors.Is(err, bolt.ErrTimeout) {
			continue
		}
		if err != nil {
			return err
		}
		err = func() error {
			defer a.Close()
			for {
				flush, cursor, ok, err := a.nextFlush(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
				if err = a.flushWithRetry(ctx, flush); err != nil {
					a.backlog.Release()
					return err
				}
				if err = a.backlog.Commit(cursor, nil); err != nil {
					return err
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func RecoverRetiredJournals(ctx context.Context, store flushStore, own string, budget int64) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ReplayRetiredJournals(ctx, store, own, budget); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
