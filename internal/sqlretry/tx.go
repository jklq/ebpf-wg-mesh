// Package sqlretry runs serializable transactions on PostgreSQL and CockroachDB.
package sqlretry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ExecuteTx retries the entire transaction with a fresh snapshot on serialization
// conflicts or deadlocks. fn must contain transactional work only; its local
// result must be reset on every invocation. Commit errors without a retryable
// SQLSTATE are returned to the caller, never blindly replayed.
func ExecuteTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	options := sql.TxOptions{Isolation: sql.LevelSerializable}
	if opts != nil {
		options = *opts
	}
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		tx, beginErr := db.BeginTx(ctx, &options)
		if beginErr != nil {
			return beginErr
		}
		err = func() error {
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err == nil {
			return nil
		}
		var state interface{ SQLState() string }
		if !errors.As(err, &state) || (state.SQLState() != "40001" && state.SQLState() != "40P01") {
			return err
		}
		if attempt == 9 {
			break
		}
		delay := min(250*time.Millisecond, time.Duration(1<<attempt)*time.Millisecond*5)
		timer := time.NewTimer(delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("transaction retry limit: %w", err)
}
