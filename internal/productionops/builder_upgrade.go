package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"os"

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
	if !verify {
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.rolling_builder_drains(plan_id STRING NOT NULL,instance STRING NOT NULL,previous_drained BOOL NOT NULL,resumed BOOL NOT NULL DEFAULT FALSE,PRIMARY KEY(plan_id,instance))`); err != nil {
			return err
		}
		if err := sqlretry.ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error {
			if args[0] == "builder-drain" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO platform_recovery.public.rolling_builder_drains(plan_id,instance,previous_drained) SELECT $1,id,drained FROM builder_workers WHERE id=$2 ON CONFLICT DO NOTHING`, r.Plan.ID, pl.Instance); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE platform_recovery.public.rolling_builder_drains q SET previous_drained=b.drained,resumed=FALSE FROM builder_workers b WHERE q.instance=b.id AND q.plan_id=$1 AND q.instance=$2 AND q.resumed`, r.Plan.ID, pl.Instance); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `UPDATE builder_workers SET drained=TRUE,updated_at=statement_timestamp() WHERE id=$1`, pl.Instance)
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE builder_workers SET drained=q.previous_drained,updated_at=statement_timestamp() FROM platform_recovery.public.rolling_builder_drains q WHERE builder_workers.id=q.instance AND q.plan_id=$1 AND q.instance=$2 AND NOT q.resumed`, r.Plan.ID, pl.Instance); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE platform_recovery.public.rolling_builder_drains SET resumed=TRUE WHERE plan_id=$1 AND instance=$2`, r.Plan.ID, pl.Instance)
			return err
		}); err != nil {
			return err
		}
	}
	var drained, previous, resumed bool
	if err := db.QueryRowContext(ctx, `SELECT b.drained,q.previous_drained,q.resumed FROM builder_workers b JOIN platform_recovery.public.rolling_builder_drains q ON q.instance=b.id WHERE q.plan_id=$1 AND b.id=$2`, r.Plan.ID, pl.Instance).Scan(&drained, &previous, &resumed); err != nil {
		return err
	}
	if args[0] == "builder-resume" {
		if !resumed || drained != previous {
			return fmt.Errorf("builder's original drain intent has not been restored")
		}
		return nil
	}
	var active int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state='running' AND builder_id=$1`, pl.Instance).Scan(&active); err != nil {
		return err
	}
	if !drained || resumed || active > 0 {
		return fmt.Errorf("builder drain is pending; active builds keep running before this replica restarts")
	}
	return nil
}

func (r *Runner) inspectBuilderUpgrade(ctx context.Context, action string) error {
	for _, op := range r.Plan.Operations {
		if op.ID == os.Getenv("PLATFORM_OPERATION") && op.Hook == action && op.Placement != nil {
			return r.builderUpgrade(ctx, []string{action, op.Placement.Instance}, true)
		}
	}
	return fmt.Errorf("builder inspection requires its deployment operation")
}
