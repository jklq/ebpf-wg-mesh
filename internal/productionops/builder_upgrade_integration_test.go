//go:build integration

package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/testdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

func TestBuilderRollingDrainKeepsActiveWorkAndOperatorIntent(t *testing.T) {
	ts, err := testserver.NewTestServer(testserver.CustomVersionOpt(testdb.DefaultVersion), testserver.SecureOpt(), testserver.StoreOnDiskOpt(), testserver.CacheSizeOpt(.02))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ts.Stop)
	if err := ts.WaitForInit(); err != nil {
		t.Fatal(err)
	}
	u := ts.PGURL()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE DATABASE platform_recovery`, `CREATE TABLE builder_workers(id STRING PRIMARY KEY,drained BOOL NOT NULL,updated_at TIMESTAMPTZ NOT NULL)`, `CREATE TABLE build_runs(id STRING PRIMARY KEY,state STRING NOT NULL,builder_id STRING NOT NULL)`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	r := testRunner(t)
	r.Config.Database.Name = u.Path[1:]
	r.Config.Database.URLFile = filepath.Join(t.TempDir(), "url")
	if err := writePrivate(r.Config.Database.URLFile, []byte(u.String())); err != nil {
		t.Fatal(err)
	}
	// Inspect real TLS SQL connectivity using the old process environment.
	bad := *u
	bad.Host = "127.0.0.1:1"
	control := *u
	q := control.Query()
	q.Set("host", "127.0.0.1,"+u.Hostname())
	q.Set("port", "1,"+u.Port())
	control.RawQuery = q.Encode()
	if err := inspectSQLFailover(context.Background(), deploy.ControlPlane, bad.Host, strings.NewReader("CONTROLPLANE_DB_URL="+control.String()+"\x00")); err != nil {
		t.Fatal("control-plane failover", err)
	}
	urls, _ := json.Marshal([]string{bad.String(), u.String()})
	if err := inspectSQLFailover(context.Background(), deploy.Console, bad.Host, strings.NewReader("DASHBOARD_DATABASE_URLS="+string(urls)+"\x00")); err != nil {
		t.Fatal("console failover", err)
	}
	only, _ := json.Marshal([]string{u.String()})
	if err := inspectSQLFailover(context.Background(), deploy.Console, u.Host, strings.NewReader("DASHBOARD_DATABASE_URLS="+string(only)+"\x00")); err == nil {
		t.Fatal("single-node client bypassed the surviving-node gate")
	}
	r.Plan.Previous = &deploy.AppliedDeployment{}
	for _, originallyDrained := range []bool{false, true} {
		id := "builder-active"
		if originallyDrained {
			id = "builder-cordoned"
		}
		pl := deploy.Placement{Role: deploy.Builder, Host: "a", Instance: id}
		r.Plan.Placements = []deploy.Placement{pl}
		r.Plan.Previous.Placements = []deploy.Placement{pl}
		if _, err := db.Exec(`INSERT INTO builder_workers VALUES ($1,$2,now()),('other-' || $1,FALSE,now())`, id, originallyDrained); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO build_runs VALUES ($1,'running',$1)`, id); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := r.builderUpgrade(ctx, []string{"builder-drain", id}, false); err == nil {
			t.Fatal("running build was interrupted")
		}
		var drained, other bool
		if err := db.QueryRow(`SELECT drained FROM builder_workers WHERE id=$1`, id).Scan(&drained); err != nil || !drained {
			t.Fatal("current worker was not drained", err)
		}
		if err := db.QueryRow(`SELECT drained FROM builder_workers WHERE id='other-' || $1`, id).Scan(&other); err != nil || other {
			t.Fatal("surviving worker was drained", err)
		}
		if _, err := db.Exec(`UPDATE build_runs SET state='succeeded' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-drain", id}, false); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-resume", id}, false); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-resume", id}, false); err != nil {
			t.Fatal("resume retry", err)
		}
		if err := db.QueryRow(`SELECT drained FROM builder_workers WHERE id=$1`, id).Scan(&drained); err != nil || drained != originallyDrained {
			t.Fatal("operator intent was lost", err)
		}
		// A later install retry must drain again and retain the current intent,
		// without overwriting it with the temporary drain on a pending retry.
		if _, err := db.Exec(`UPDATE builder_workers SET drained=$2 WHERE id=$1`, id, !originallyDrained); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE build_runs SET state='running' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-drain", id}, false); err == nil {
			t.Fatal("resumed builder bypassed the drain on its next install")
		}
		if _, err := db.Exec(`UPDATE build_runs SET state='succeeded' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-drain", id}, false); err != nil {
			t.Fatal(err)
		}
		if err := r.builderUpgrade(ctx, []string{"builder-resume", id}, false); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT drained FROM builder_workers WHERE id=$1`, id).Scan(&drained); err != nil || drained == originallyDrained {
			t.Fatal("install retry lost the updated operator intent", err)
		}
	}
}
