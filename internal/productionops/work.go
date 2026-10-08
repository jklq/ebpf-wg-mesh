package productionops

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/deploy"
	"fmt"
)

func (r *Runner) recoverWork(ctx context.Context, verify bool) error {
	if _, err := r.report(true); err != nil {
		return err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if !verify {
		// Preserve the exact records (including idempotency keys and payloads) before
		// removing their restored ownership. Unknown external effects are quarantined
		// rather than reissued. Operators can inspect them through direct SQL offline.
		if _, err := db.ExecContext(ctx, `CREATE DATABASE IF NOT EXISTS platform_recovery`); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.work_quarantine(generation STRING,kind STRING,id STRING,record JSONB NOT NULL,PRIMARY KEY(generation,kind,id))`); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, item := range []struct{ table, where string }{{"build_runs", "state='running'"}, {"build_attempts", "outcome='leased'"}, {"builder_workers", "TRUE"}, {"durable_work_items", "state IN ('pending','leased','failed')"}, {"github_work_items", "state NOT IN ('completed','failed','quarantined')"}, {"github_webhook_deliveries", "state NOT IN ('processed','failed','quarantined')"}, {"registry_image_deletions", "TRUE"}} {
			statement := `INSERT INTO platform_recovery.public.work_quarantine(generation,kind,id,record) SELECT $1,$2,` + "id" + `,to_jsonb(t) FROM ` + item.table + ` t WHERE ` + item.where + ` ON CONFLICT DO NOTHING`
			if item.table == "registry_image_deletions" {
				statement = `INSERT INTO platform_recovery.public.work_quarantine(generation,kind,id,record) SELECT $1,$2,image_ref,to_jsonb(t) FROM registry_image_deletions t ON CONFLICT DO NOTHING`
			}
			if _, err := tx.ExecContext(ctx, statement, r.Plan.Generation, item.table); err != nil {
				return err
			}
		}
		for _, statement := range []string{
			`UPDATE build_runs SET state='failed',builder_id=NULL,owner_epoch=owner_epoch+1,lease_expires_at=NULL,failure_reason='recovery invalidated stale ownership',finished_at=statement_timestamp() WHERE state='running'`,
			`UPDATE build_attempts SET outcome='recovery-invalidated',detail='external build effects quarantined',finished_at=statement_timestamp() WHERE outcome='leased'`,
			`UPDATE builder_workers SET current_build_id='',drained=TRUE,updated_at=statement_timestamp() WHERE current_build_id<>'' OR NOT drained`,
			`UPDATE durable_work_items SET state='dead',owner_id='',owner_epoch=owner_epoch+1,lease_expires_at=NULL,last_error='recovery quarantined uncertain external effects',updated_at=statement_timestamp() WHERE state IN ('pending','leased','failed')`,
			`UPDATE github_work_items SET state='quarantined',processor_id='',last_error='recovery quarantined uncertain external effects',updated_at=statement_timestamp() WHERE state NOT IN ('completed','failed','quarantined')`,
			`UPDATE github_webhook_deliveries SET state='quarantined',processor_id='',last_error='recovery quarantined uncertain external effects',updated_at=statement_timestamp() WHERE state NOT IN ('processed','failed','quarantined')`,
			`DELETE FROM registry_image_deletions`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	var unsafe int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM build_runs WHERE state='running' OR lease_expires_at IS NOT NULL) + (SELECT count(*) FROM build_attempts WHERE outcome='leased') + (SELECT count(*) FROM builder_workers WHERE current_build_id<>'') + (SELECT count(*) FROM durable_work_items WHERE state IN ('pending','leased','failed') OR owner_id<>'' OR lease_expires_at IS NOT NULL) + (SELECT count(*) FROM github_work_items WHERE state NOT IN ('completed','failed','quarantined') OR processor_id<>'') + (SELECT count(*) FROM github_webhook_deliveries WHERE state NOT IN ('processed','failed','quarantined') OR processor_id<>'') + (SELECT count(*) FROM registry_image_deletions)`).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe != 0 {
		return fmt.Errorf("stale work ownership or uncertain external effects remain active")
	}
	// Preserve every field not deliberately changed to invalidate ownership.
	// This includes source snapshots, external request payloads and idempotency
	// identities; an empty queue alone is not successful work recovery.
	for table, changed := range map[string][]string{
		"build_runs":                {"state", "builder_id", "owner_epoch", "lease_expires_at", "failure_reason", "finished_at"},
		"build_attempts":            {"outcome", "detail", "finished_at"},
		"builder_workers":           {"current_build_id", "drained", "updated_at"},
		"durable_work_items":        {"state", "owner_id", "owner_epoch", "lease_expires_at", "last_error", "updated_at"},
		"github_work_items":         {"state", "processor_id", "last_error", "updated_at"},
		"github_webhook_deliveries": {"state", "processor_id", "last_error", "updated_at"},
	} {
		left, right := "to_jsonb(w)", "q.record"
		for _, column := range changed {
			left += "-'" + column + "'"
			right += "-'" + column + "'"
		}
		var missing int
		statement := `SELECT count(*) FROM platform_recovery.public.work_quarantine q LEFT JOIN ` + table + ` w ON w.id=q.id WHERE q.generation=$1 AND q.kind=$2 AND (w.id IS NULL OR (` + left + `) <> (` + right + `))`
		if err := db.QueryRowContext(ctx, statement, r.Plan.Generation, table).Scan(&missing); err != nil {
			return err
		}
		if missing != 0 {
			return fmt.Errorf("quarantined %s dependencies were lost or changed", table)
		}
	}
	return r.verifyPause(ctx, true, true)
}
func (r *Runner) retireAgent(ctx context.Context, db *sql.DB, id string) error {
	return adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.AdministrationRow(id).Exec(ctx, tx, `UPDATE agent_administration SET lifecycle_state='retired',operator_intent='cordoned',credential_revoked_at=COALESCE(credential_revoked_at,statement_timestamp()),updated_at=statement_timestamp() WHERE agent_id=$1`, id)
		return err
	})
}

// Restore the selected live builders' operator intent only after stale lease
// ownership has been invalidated. Unknown/restored workers remain drained.
func (r *Runner) admitBuilders(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Builder {
			continue
		}
		_, err = db.ExecContext(ctx, `UPDATE builder_workers SET drained=COALESCE((SELECT (record->>'drained')::BOOL FROM platform_recovery.public.work_quarantine WHERE generation=$1 AND kind='builder_workers' AND id=$2),FALSE),current_build_id='',updated_at=statement_timestamp() WHERE id=$2`, r.Plan.Generation, pl.Instance)
		if err != nil {
			return err
		}
	}
	return nil
}
