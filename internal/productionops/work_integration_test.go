//go:build integration

package productionops

import (
	"context"
	"database/sql"
	"testing"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/recovery"
)

func testNativeWorkQuarantine(t *testing.T, ctx context.Context, r *Runner, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO projects(id,name,kind,owner_user_id,created_at) VALUES('work-project','work','user','operator',now())`,
		`INSERT INTO environments(id,project_id,name,kind,auto_deploy,network_identity,created_at,updated_at) VALUES('work-environment','work-project','work','user',false,800,now(),now())`,
		`INSERT INTO services(id,environment_id,name,current_spec_revision,created_at,updated_at) VALUES('work-service','work-environment','work',1,now(),now())`,
		`INSERT INTO builder_workers(id,name,current_build_id,last_heartbeat_at,created_at,updated_at) VALUES('old-builder','old','leased-build',now(),now(),now())`,
		`INSERT INTO build_runs(id,service_id,commit_sha,state,builder_id,owner_epoch,lease_expires_at,queued_at,source_snapshot_digest,build_recipe_json) VALUES('leased-build','work-service','exact-commit','running','old-builder',7,now()+INTERVAL '1 hour',now(),'sha256:source','{"source":"preserve"}')`,
		`INSERT INTO build_attempts(id,build_id,attempt_number,builder_id,owner_epoch,started_at) VALUES('attempt','leased-build',1,'old-builder',7,now())`,
		`INSERT INTO durable_work_items(id,kind,dedup_key,state,attempt_limit,owner_id,owner_epoch,lease_expires_at,available_at,payload,created_at,updated_at) VALUES('durable','effect','durable-idempotency','leased',3,'old-worker',9,now()+INTERVAL '1 hour',now(),'{"external":"preserve"}',now(),now())`,
		`INSERT INTO github_work_items(id,kind,state,processor_id,idempotency_key,commit_sha,available_at,created_at,updated_at) VALUES('github','check','processing','old-worker','github-idempotency','exact-commit',now(),now(),now())`,
		`INSERT INTO github_webhook_deliveries(id,delivery_id,event_type,state,processor_id,payload,received_at,updated_at) VALUES('webhook','delivery-idempotency','push','processing','old-worker','{"source":"preserve"}',now(),now())`,
		`INSERT INTO registry_image_deletions(image_ref,created_at) VALUES('registry/image@sha256:protected',now())`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	r.Plan.Recovery = true
	report := &recovery.FleetReport{Installation: r.Plan.Installation.ID, Generation: r.Plan.Generation}
	r.RecoveryProgress = &deploy.RecoveryProgress{Generation: r.Plan.Generation, Report: report, ApprovedDigest: report.ApprovalDigest(), ReservedDigest: report.ApprovalDigest()}
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.recoverWork(ctx, false); err != nil {
			t.Fatal("work recovery/retry", err)
		}
		if err := r.recoverWork(ctx, true); err != nil {
			t.Fatal("work verification", err)
		}
	}
	var epoch int64
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT owner_epoch,payload->>'external' FROM durable_work_items WHERE id='durable'`).Scan(&epoch, &payload); err != nil {
		t.Fatal(err)
	}
	if epoch != 10 || payload != "preserve" {
		t.Fatal("retry changed ownership twice or lost external payload", epoch, payload)
	}
	var records int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM platform_recovery.public.work_quarantine WHERE generation=$1`, r.Plan.Generation).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 7 {
		t.Fatal("missing exact work quarantine records", records)
	}
	result, err := db.ExecContext(ctx, `UPDATE durable_work_items SET state='succeeded' WHERE id='durable' AND state='leased' AND owner_id='old-worker' AND owner_epoch=9`)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 0 {
		t.Fatal("old owner completed invalidated work", changed, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE github_webhook_deliveries SET payload='{"source":"lost"}' WHERE id='webhook'`); err != nil {
		t.Fatal(err)
	}
	if err := r.recoverWork(ctx, true); err == nil {
		t.Fatal("lost source payload passed verification")
	}
}
