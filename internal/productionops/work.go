package productionops

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
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
	// Compare every quarantine record to a surviving row. A missing source or
	// idempotency key is a failed recovery, even when its queue happens to be empty.
	var missing int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM platform_recovery.public.work_quarantine q LEFT JOIN durable_work_items w ON w.id=q.id WHERE q.generation=$1 AND q.kind='durable_work_items' AND (w.id IS NULL OR w.dedup_key<>q.record->>'dedup_key' OR w.payload<>q.record->'payload')`, r.Plan.Generation).Scan(&missing); err != nil {
		return err
	}
	if missing != 0 {
		return fmt.Errorf("quarantined work dependencies were lost")
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM platform_recovery.public.work_quarantine q LEFT JOIN github_work_items w ON w.id=q.id WHERE q.generation=$1 AND q.kind='github_work_items' AND (w.id IS NULL OR w.idempotency_key<>q.record->>'idempotency_key')`, r.Plan.Generation).Scan(&missing); err != nil {
		return err
	}
	if missing != 0 {
		return fmt.Errorf("external idempotency identities were lost")
	}
	return nil
}
func (r *Runner) retireAgent(ctx context.Context, db *sql.DB, id string) error {
	return adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.AdministrationRow(id).Exec(ctx, tx, `UPDATE agent_administration SET lifecycle_state='retired',operator_intent='cordoned',credential_revoked_at=COALESCE(credential_revoked_at,statement_timestamp()),updated_at=statement_timestamp() WHERE agent_id=$1`, id)
		return err
	})
}
