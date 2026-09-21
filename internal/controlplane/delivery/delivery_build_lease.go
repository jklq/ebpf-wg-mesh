package delivery

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
)

// HeartbeatBuild extends the lease on a running build. It is a single
// compare-and-swap on (builder_id, owner_epoch): a superseded owner gets
// ErrBuildLeaseLost and must drop the work without completing it. A build
// with cancellation requested gets ErrBuildCancelled so the builder stops
// cooperatively; the late completion path then converges to cancelled.
func (d *Delivery) HeartbeatBuild(ctx context.Context, builderID, buildID string, epoch int64) error {
	scheduler := d.BuildSchedulerConfig()
	return d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		var cancelRequested bool
		if err := tx.QueryRowContext(ctx,
			`SELECT cancel_requested_at IS NOT NULL FROM build_runs WHERE id = $1 FOR UPDATE`, buildID,
		).Scan(&cancelRequested); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBuildNotOwned
			}
			return err
		}
		if cancelRequested {
			return ErrBuildCancelled
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE build_runs
			    SET lease_expires_at = $1,
			        last_heartbeat_at = $2
			  WHERE id = $3 AND state = $4 AND builder_id = $5 AND owner_epoch = $6`,
			now.Add(scheduler.LeaseTTL), now, buildID, BuildStateRunning, builderID, epoch,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrBuildLeaseLost
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE builder_workers SET current_build_id = $1, last_heartbeat_at = $2, updated_at = $2
			  WHERE id = $3`,
			buildID, now, builderID,
		); err != nil {
			return err
		}
		return nil
	})
}

// BuildAttempts returns the attempt history for one build, oldest first.
func (d *Delivery) BuildAttempts(ctx context.Context, user authz.User, serviceID, buildID string) ([]BuildAttemptRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	var serviceOfBuild string
	if err := d.store.db.QueryRowContext(ctx, `SELECT service_id FROM build_runs WHERE id = $1`, buildID).Scan(&serviceOfBuild); err != nil {
		return nil, err
	}
	if serviceOfBuild != scope.ID() {
		return nil, sql.ErrNoRows
	}
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT id, build_id, attempt_number, builder_id, owner_epoch, started_at, finished_at, outcome, detail
		   FROM build_attempts WHERE build_id = $1 ORDER BY attempt_number ASC, id ASC`, buildID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildAttemptRecord
	for rows.Next() {
		rec, err := scanBuildAttemptRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListBuilders returns builder worker records. Operators only.
func (d *Delivery) ListBuilders(ctx context.Context, user authz.User) ([]BuilderWorkerRecord, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return nil, err
	}
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT id, name, current_build_id, last_heartbeat_at, drained, updated_at FROM builder_workers ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuilderWorkerRecord
	for rows.Next() {
		var rec BuilderWorkerRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.CurrentBuildID, &rec.LastHeartbeat, &rec.Drained, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetBuilderDrain marks a builder drained (no new claims; running work
// finishes) or undrained. Operators only.
func (d *Delivery) SetBuilderDrain(ctx context.Context, user authz.User, builderID string, drained bool) (BuilderWorkerRecord, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuilderWorkerRecord{}, err
	}
	builderID = strings.TrimSpace(builderID)
	if builderID == "" {
		return BuilderWorkerRecord{}, errors.New("builder id is required")
	}
	var rec BuilderWorkerRecord
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE builder_workers SET drained = $1, updated_at = $2 WHERE id = $3`,
			drained, now, builderID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return sql.ErrNoRows
		}
		return tx.QueryRowContext(ctx,
			`SELECT id, name, current_build_id, last_heartbeat_at, drained, updated_at FROM builder_workers WHERE id = $1`,
			builderID,
		).Scan(&rec.ID, &rec.Name, &rec.CurrentBuildID, &rec.LastHeartbeat, &rec.Drained, &rec.UpdatedAt)
	})
	return rec, err
}

// BuildSchedulerState returns the pause flag plus queue counters. Operators only.
func (d *Delivery) BuildSchedulerState(ctx context.Context, user authz.User) (BuildSchedulerState, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuildSchedulerState{}, err
	}
	scheduler := d.BuildSchedulerConfig()
	var state BuildSchedulerState
	state.MaxConcurrentGlobal = scheduler.MaxConcurrentGlobal
	state.MaxConcurrentPerProject = scheduler.MaxConcurrentPerProject
	if err := d.store.db.QueryRowContext(ctx, `SELECT paused, updated_at FROM build_scheduler_control WHERE id = TRUE`).Scan(&state.Paused, &state.UpdatedAt); err != nil {
		return BuildSchedulerState{}, err
	}
	if err := d.store.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE state = $1), count(*) FILTER (WHERE state = $2) FROM build_runs`,
		BuildStateRunning, BuildStateQueued,
	).Scan(&state.RunningBuilds, &state.QueuedBuilds); err != nil {
		return BuildSchedulerState{}, err
	}
	return state, nil
}

// SetBuildSchedulerPaused pauses (no new claims; running work finishes) or
// resumes the build queue. Operators only.
func (d *Delivery) SetBuildSchedulerPaused(ctx context.Context, user authz.User, paused bool) (BuildSchedulerState, error) {
	if _, err := d.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return BuildSchedulerState{}, err
	}
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE build_scheduler_control SET paused = $1, updated_at = $2 WHERE id = TRUE`, paused, now)
		return err
	})
	if err != nil {
		return BuildSchedulerState{}, err
	}
	return d.BuildSchedulerState(ctx, user)
}
