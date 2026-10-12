package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/sqlretry"
)

// Drain only this replica, preserving an operator's existing drain intent.
func (r *Runner) builderUpgrade(ctx context.Context, args []string, verify bool) error {
	if len(args) != 2 || (args[0] != "builder-drain" && args[0] != "builder-resume") {
		return fmt.Errorf("builder upgrade requires drain/resume and an instance")
	}
	pl, _, err := r.findPlacement(deploy.Builder, args[1])
	if err != nil {
		return err
	}
	if r.Plan.Previous == nil {
		return fmt.Errorf("builder upgrade requires a previous deployment")
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureBuilderDrainTable(ctx, db); err != nil {
		return err
	}
	if !verify {
		if args[0] == "builder-drain" {
			// An interrupted credential restart must not become the operator's intent.
			if err := resumeBuilder(ctx, db, credentialDrainKey, pl.Instance); err != nil {
				return err
			}
			err = drainBuilder(ctx, db, r.Plan.ID, pl.Instance)
		} else {
			err = resumeBuilder(ctx, db, r.Plan.ID, pl.Instance)
		}
		if err != nil {
			return err
		}
	}
	var drained, previous, resumed bool
	var drainedAt sql.NullTime
	var now time.Time
	if err := db.QueryRowContext(ctx, `SELECT b.drained,q.previous_drained,q.resumed,q.drained_at,statement_timestamp() FROM builder_workers b JOIN platform_recovery.public.rolling_builder_drains q ON q.instance=b.id WHERE q.plan_id=$1 AND b.id=$2`, r.Plan.ID, pl.Instance).Scan(&drained, &previous, &resumed, &drainedAt, &now); err != nil {
		return err
	}
	if args[0] == "builder-resume" {
		if !resumed || drained != previous {
			return fmt.Errorf("builder's original drain intent has not been restored")
		}
		return nil
	}
	active, err := activeBuilds(ctx, db, pl.Instance)
	if err != nil {
		return err
	}
	deadline := time.Duration(r.Plan.DrainDeadlineSeconds) * time.Second
	if !drained || resumed || builderDrainBlocking(active, drainedAt, now, deadline) {
		return fmt.Errorf("builder drain is pending; active builds keep running before this replica restarts")
	}
	return nil
}

// builderDrainBlocking reports whether active builds still hold a drained
// builder's restart. Without a plan drain deadline they hold it indefinitely
// and nothing is forced off; with one, the restart proceeds once the builder
// has been drained for that long and the running builds are interrupted.
func builderDrainBlocking(active int, drainedAt sql.NullTime, now time.Time, deadline time.Duration) bool {
	if active == 0 {
		return false
	}
	if deadline <= 0 || !drainedAt.Valid {
		return true
	}
	return now.Sub(drainedAt.Time) < deadline
}

func (r *Runner) inspectBuilderUpgrade(ctx context.Context, action string) error {
	for _, op := range r.Plan.Operations {
		if op.ID == os.Getenv("PLATFORM_OPERATION") && op.Hook == action && op.Placement != nil {
			return r.builderUpgrade(ctx, []string{action, op.Placement.Instance}, true)
		}
	}
	return fmt.Errorf("builder inspection requires its deployment operation")
}

const builderDrainTable = `CREATE TABLE IF NOT EXISTS platform_recovery.public.rolling_builder_drains(plan_id STRING NOT NULL,instance STRING NOT NULL,previous_drained BOOL NOT NULL,resumed BOOL NOT NULL DEFAULT FALSE,drained_at TIMESTAMPTZ,PRIMARY KEY(plan_id,instance))`

// Tables created before drained_at was recorded gain the column in place.
const builderDrainDrainedAt = `ALTER TABLE platform_recovery.public.rolling_builder_drains ADD COLUMN IF NOT EXISTS drained_at TIMESTAMPTZ`

func ensureBuilderDrainTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, builderDrainTable); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, builderDrainDrainedAt)
	return err
}

// drainBuilder records the builder's drain intent once per key, then drains it.
// A key that was resumed records the intent again for its next drain. The drain
// start is recorded once, when the drain begins, so a plan drain deadline is
// measured from the drain and not from the latest verification retry.
func drainBuilder(ctx context.Context, db *sql.DB, key, instance string) error {
	if err := ensureBuilderDrainTable(ctx, db); err != nil {
		return err
	}
	return sqlretry.ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO platform_recovery.public.rolling_builder_drains(plan_id,instance,previous_drained,drained_at) SELECT $1,id,drained,statement_timestamp() FROM builder_workers WHERE id=$2 ON CONFLICT DO NOTHING`, key, instance); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE platform_recovery.public.rolling_builder_drains q SET previous_drained=b.drained,resumed=FALSE,drained_at=statement_timestamp() FROM builder_workers b WHERE q.instance=b.id AND q.plan_id=$1 AND q.instance=$2 AND q.resumed`, key, instance); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE builder_workers SET drained=TRUE,updated_at=statement_timestamp() WHERE id=$1`, instance)
		return err
	})
}

// resumeBuilder restores the drain intent recorded under key, if still drained.
func resumeBuilder(ctx context.Context, db *sql.DB, key, instance string) error {
	if err := ensureBuilderDrainTable(ctx, db); err != nil {
		return err
	}
	return sqlretry.ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE builder_workers SET drained=q.previous_drained,updated_at=statement_timestamp() FROM platform_recovery.public.rolling_builder_drains q WHERE builder_workers.id=q.instance AND q.plan_id=$1 AND q.instance=$2 AND NOT q.resumed`, key, instance); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE platform_recovery.public.rolling_builder_drains SET resumed=TRUE WHERE plan_id=$1 AND instance=$2`, key, instance)
		return err
	})
}

func activeBuilds(ctx context.Context, db *sql.DB, instance string) (int, error) {
	var active int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state='running' AND builder_id=$1`, instance).Scan(&active)
	return active, err
}
