//go:build integration

package productionops

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/testdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

func TestNativeRestoreJobDiscoveryPreservesCompletedDestination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir, planID := t.TempDir(), strings.Repeat("a1", 32)
	db, stop := testserver.NewDBForTest(t, testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	defer stop()
	collection := "nodelocal://1/" + planID + "/database"
	if _, err := db.ExecContext(ctx, `CREATE TABLE recovery_effect(value STRING); INSERT INTO recovery_effect VALUES ('selected')`); err != nil {
		t.Fatal(err)
	}
	var cutoff time.Time
	if err := db.QueryRowContext(ctx, `SELECT date_trunc('microsecond',clock_timestamp())`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `BACKUP INTO '`+collection+`' AS OF SYSTEM TIME '`+cutoff.UTC().Format(time.RFC3339Nano)+`' WITH revision_history`); err != nil {
		t.Fatal(err)
	}
	var subdirectory string
	if err := db.QueryRowContext(ctx, `SELECT path FROM [SHOW BACKUPS IN '`+collection+`']`).Scan(&subdirectory); err != nil {
		t.Fatal(err)
	}
	restored, stopRestored := testserver.NewDBForTestWithDatabase(t, "system", testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.CacheSizeOpt(.02), testserver.ExternalIODirOpt(dir))
	defer stopRestored()
	if _, _, err := nativeRestoreJob(ctx, restored, planID, subdirectory, cutoff); err != sql.ErrNoRows {
		t.Fatal("empty native destination unexpectedly has a restore job", err)
	}
	var submitted int64
	if err := restored.QueryRowContext(ctx, `RESTORE FROM '`+subdirectory+`' IN '`+collection+`' AS OF SYSTEM TIME '`+cutoff.UTC().Format(time.RFC3339Nano)+`' WITH detached`).Scan(&submitted); err != nil {
		t.Fatal(err)
	}
	for {
		job, status, err := nativeRestoreJob(ctx, restored, planID, subdirectory, cutoff)
		if err != nil || job != submitted {
			t.Fatal("durable native job discovery", job, status, err)
		}
		if status == "succeeded" {
			break
		}
		if status == "failed" || status == "canceled" || status == "paused" {
			t.Fatal("native restore did not succeed", status)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Reopen connections as an interrupted process would. Discovery must retain
	// the completed destination rather than infer absence from abbreviated text.
	restored.SetMaxIdleConns(0)
	for attempt := 0; attempt < 2; attempt++ {
		job, status, err := nativeRestoreJob(ctx, restored, planID, subdirectory, cutoff)
		if err != nil || job != submitted || status != "succeeded" {
			t.Fatal("completed restore retry", job, status, err)
		}
		var value string
		if err := restored.QueryRowContext(ctx, `SELECT value FROM defaultdb.public.recovery_effect`).Scan(&value); err != nil || value != "selected" {
			t.Fatal("retry lost restored native state", value, err)
		}
	}
	if _, _, err := nativeRestoreJob(ctx, restored, planID, subdirectory, cutoff.Add(time.Microsecond)); err == nil || err == sql.ErrNoRows {
		t.Fatal("existing job with a different exact timestamp was treated as absent", err)
	}
	if _, _, err := nativeRestoreJob(ctx, restored, planID, "/another-backup", cutoff); err == nil || err == sql.ErrNoRows {
		t.Fatal("existing job with a different selected backup was treated as absent", err)
	}
}
