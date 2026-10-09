//go:build integration

package recovery

import (
	"context"
	"testing"
	"time"

	"ebof-wg-mesh/internal/testdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

func TestNativeSchedulesAndTimestampedRecoveryInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	db, stop := testserver.NewDBForTest(t, testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.StoreOnDiskOpt(), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	defer stop()
	for _, statement := range []string{
		`CREATE EXTERNAL CONNECTION recovery_test AS 'nodelocal://1/recovery'`,
		`CREATE TABLE schema_migrations(version INT PRIMARY KEY); INSERT INTO schema_migrations VALUES (42)`,
		`CREATE SCHEMA dashboard; CREATE TABLE dashboard.schema_migrations(version INT PRIMARY KEY); INSERT INTO dashboard.schema_migrations VALUES (3)`,
		`CREATE TABLE envelope_keys(id STRING DEFAULT 'k' PRIMARY KEY,provider_ref STRING); INSERT INTO envelope_keys(provider_ref) VALUES ('k1')`,
		`CREATE TABLE envelope_data_keys(id STRING,wrapping_key_id STRING,wrapped_dek BYTES)`,
		`CREATE TABLE dashboard.accounts(user_id STRING,provider_subject STRING,access_token STRING,refresh_token STRING,provider STRING)`,
		`CREATE TABLE source_snapshots(digest STRING,object_key STRING); INSERT INTO source_snapshots VALUES ('sha256:abc','archive-original')`,
		`CREATE TABLE source_archive_objects(digest STRING,object_key STRING)`,
		`CREATE TABLE build_runs(source_snapshot_digest STRING,state STRING)`,
		`CREATE TABLE build_artifacts(kind STRING,image_ref STRING,image_manifest_digest STRING,image_retained BOOL); INSERT INTO build_artifacts VALUES ('direct_image','external.example/a@sha256:123','sha256:123',true)`,
		`CREATE TABLE agent_registrations(id STRING PRIMARY KEY); INSERT INTO agent_registrations VALUES ('agent-original')`,
		`CREATE TABLE ingress_nodes(node_id STRING PRIMARY KEY); INSERT INTO ingress_nodes VALUES ('ingress-original')`,
		`CREATE TABLE platform_signing_keys(id STRING PRIMARY KEY,wrapping_key_id STRING,wrapped_key BYTES); INSERT INTO platform_signing_keys(id) VALUES ('signing-original')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := Register(ctx, db, "installation", "release1", []Requirement{{Kind: "release", ID: "release1"}}); err != nil {
		t.Fatal(err)
	}
	connection, err := externalConnectionURI(ctx, db, "external://recovery_test")
	if err != nil || connection.Scheme != "nodelocal" || connection.Path != "/recovery" {
		t.Fatal("supported external connection inspection failed", err)
	}
	var timestamp, fullTimestamp time.Time
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&fullTimestamp); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM source_snapshots; UPDATE agent_registrations SET id='agent-new'; UPDATE envelope_keys SET provider_ref='k2'`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(ctx, db, "installation", "dashboard", timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Requirements) != 3 || snapshot.Identities["agent_registrations"][0] != "agent-original" || len(snapshot.ExternalImages) != 1 {
		t.Fatal("inventory did not use one database timestamp", snapshot)
	}
	statement, err := ScheduleSQL("installation", "external://recovery_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchedules(ctx, db, "installation", "external://recovery_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "SET CLUSTER SETTING jobs.scheduler.enabled=false"); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchedules(ctx, db, "installation", "external://recovery_test"); err == nil {
		t.Fatal("disabled native scheduler accepted")
	}
	if _, err := db.ExecContext(ctx, "SET CLUSTER SETTING jobs.scheduler.enabled=true"); err != nil {
		t.Fatal(err)
	}
	// Pause both schedules so an explicit backup exercises the supported native
	// inspection without racing the automatic initial backup during this test.
	if _, err := db.ExecContext(ctx, `PAUSE SCHEDULES SELECT id FROM [SHOW SCHEDULES] WHERE label='recovery-installation'`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchedules(ctx, db, "installation", "external://recovery_test"); err == nil {
		t.Fatal("paused schedules accepted")
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEDULE 'operator-owned' FOR BACKUP INTO 'external://recovery_test' RECURRING '@daily' WITH SCHEDULE OPTIONS first_run='2100-01-01'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PAUSE SCHEDULES SELECT id FROM [SHOW SCHEDULES] WHERE label='operator-owned'`); err != nil {
		t.Fatal(err)
	}
	const unrelatedQuery = `SELECT string_agg(id::STRING,',' ORDER BY id) FROM [SHOW SCHEDULES] WHERE label='operator-owned' AND schedule_status='PAUSED'`
	var unrelatedBefore string
	if err := db.QueryRowContext(ctx, unrelatedQuery).Scan(&unrelatedBefore); err != nil || unrelatedBefore == "" {
		t.Fatal("unrelated paused schedules missing", err)
	}
	if err := ReestablishSchedules(ctx, db, "installation", "external://wrong_destination"); err == nil {
		t.Fatal("recovery replaced a pair with an unverified destination")
	}
	if err := VerifySchedules(ctx, db, "installation", "external://recovery_test"); err == nil {
		t.Fatal("rejected recovery changed the paused pair")
	}
	if err := ReestablishSchedules(ctx, db, "installation", "external://recovery_test"); err != nil {
		t.Fatal("recovery failed to re-establish the paused native pair", err)
	}
	var fullID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM [SHOW SCHEDULES] WHERE label='recovery-installation' AND recurrence=$1`, FullCron).Scan(&fullID); err != nil {
		t.Fatal(err)
	}
	if err := ReestablishSchedules(ctx, db, "installation", "external://recovery_test"); err != nil {
		t.Fatal("re-establishment retry", err)
	}
	var retryID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM [SHOW SCHEDULES] WHERE label='recovery-installation' AND recurrence=$1`, FullCron).Scan(&retryID); err != nil || retryID != fullID {
		t.Fatal("retry replaced the active native pair", err)
	}
	var unrelatedAfter string
	if err := db.QueryRowContext(ctx, unrelatedQuery).Scan(&unrelatedAfter); err != nil || unrelatedAfter != unrelatedBefore {
		t.Fatal("unrelated paused schedules were changed", err)
	}
	// Interruption after dropping the old pair must recover from actual absence.
	if _, err := db.ExecContext(ctx, `DROP SCHEDULES SELECT id FROM [SHOW SCHEDULES] WHERE label='recovery-installation'`); err != nil {
		t.Fatal(err)
	}
	if err := ReestablishSchedules(ctx, db, "installation", "external://recovery_test"); err != nil {
		t.Fatal("re-establishment after interrupted removal", err)
	}
	// Keep the following explicit backup independent of the automatic jobs.
	if _, err := db.ExecContext(ctx, `PAUSE SCHEDULES SELECT id FROM [SHOW SCHEDULES] WHERE label='recovery-installation'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `BACKUP INTO 'external://recovery_test' AS OF SYSTEM TIME `+literal(fullTimestamp.Format(time.RFC3339Nano))+` WITH revision_history`); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := db.QueryRowContext(ctx, `SELECT path FROM [SHOW BACKUPS IN 'external://recovery_test'] ORDER BY path DESC LIMIT 1`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	chain, err := InspectBackup(ctx, db, "external://recovery_test", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Layers) != 1 || !chain.Layers[0].Start.IsZero() {
		t.Fatal("full native backup inventory mismatch", chain)
	}
	if _, err := db.ExecContext(ctx, `BACKUP INTO LATEST IN 'external://recovery_test' WITH revision_history`); err != nil {
		t.Fatal(err)
	}
	chain, err = InspectBackup(ctx, db, "external://recovery_test", path)
	if err != nil || len(chain.Layers) != 2 || !chain.Layers[1].Start.Equal(chain.Layers[0].End) {
		t.Fatal("native incremental chain", chain, err)
	}
	if !timestamp.After(chain.Layers[0].End) || !timestamp.Before(chain.Layers[1].End) {
		t.Fatal("test did not select a timestamp between backup endpoints")
	}
	// Re-establish into the actual nonempty collection after site loss. Native
	// schedule creation requires acknowledgment of its protected old backups.
	if err := ReestablishSchedules(ctx, db, "installation", "external://recovery_test"); err != nil {
		t.Fatal("re-establish native schedules over historical backups", err)
	}
	preserved, err := InspectBackup(ctx, db, "external://recovery_test", path)
	if err != nil || len(preserved.Layers) != len(chain.Layers) || !preserved.Layers[1].End.Equal(chain.Layers[1].End) {
		t.Fatal("schedule recovery changed the protected backup chain", err)
	}
	restored, stopRestore := testserver.NewDBForTest(t, testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	defer stopRestore()
	if _, err := restored.ExecContext(ctx, `RESTORE FROM `+literal(path)+` IN 'nodelocal://1/recovery' AS OF SYSTEM TIME `+literal(timestamp.Format(time.RFC3339Nano))); err != nil {
		t.Fatal("native timestamped restore", err)
	}
	var agent, archive, version string
	if err := restored.QueryRowContext(ctx, `SELECT id FROM agent_registrations`).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueryRowContext(ctx, `SELECT object_key FROM source_snapshots`).Scan(&archive); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueryRowContext(ctx, `SELECT provider_ref FROM envelope_keys`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if agent != "agent-original" || archive != "archive-original" || version != "k1" {
		t.Fatal("restore lost timestamp identity", agent, archive, version)
	}
	// Completion selects the latest inspected endpoint. Unlike an interior
	// timestamp, rounding that endpoint forward can put it outside the backup.
	endpoint, stopEndpoint := testserver.NewDBForTest(t, testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	defer stopEndpoint()
	cutoff := chain.Layers[len(chain.Layers)-1].End
	if _, err := endpoint.ExecContext(ctx, `RESTORE FROM `+literal(path)+` IN 'nodelocal://1/recovery' AS OF SYSTEM TIME `+literal(cutoff.Format(time.RFC3339Nano))); err != nil {
		t.Fatal("native exact inspected endpoint restore", cutoff, err)
	}
	if err := endpoint.QueryRowContext(ctx, `SELECT id FROM agent_registrations`).Scan(&agent); err != nil || agent != "agent-new" {
		t.Fatal("native endpoint restore did not retain actual cutoff data", agent, err)
	}
}
