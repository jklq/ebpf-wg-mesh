//go:build integration

package productionops

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/testdb"
)

func TestBuilderRollingDrainKeepsActiveWorkAndOperatorIntent(t *testing.T) {
	binDir, ok := testdb.Binaries()
	if !ok {
		t.Skip("set POSTGRES_BIN_DIR to local PostgreSQL binaries")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := testRunner(t)
	r.Config.Database.PostgresBinary = filepath.Join(binDir, "postgres")
	r.Config.Database.InitDBBinary = filepath.Join(binDir, "initdb")
	r.Config.Database.Address = freeAddress(t, "127.0.0.1")
	r.Plan.Installation.Hosts[0].Network.Address = "127.0.0.1"
	if err := r.databaseCredentials(ctx, false); err != nil {
		t.Fatal(err)
	}
	pl := r.Plan.Placements[0]
	host := r.Plan.Installation.Hosts[0]
	remote := r.Remote.(isolatedRemote)
	certs := remote.path(host, cfgDir(r.Plan, pl)+"/certs")
	data := remote.path(host, dataDir(r.Plan, pl)+"/data")
	logs, err := os.Create(r.Config.StateDirectory + "/postgres-builder.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	_, port, _ := net.SplitHostPort(r.Config.Database.Address)
	process := exec.CommandContext(ctx, r.Config.Database.PostgresBinary,
		"-D", data, "-c", "listen_addresses=127.0.0.1", "-c", "port="+port, "-c", "unix_socket_directories="+r.Config.StateDirectory,
		"-c", "ssl=on", "-c", "ssl_cert_file="+certs+"/node.crt", "-c", "ssl_key_file="+certs+"/node.key",
		"-c", "ssl_ca_file="+certs+"/ca.crt", "-c", "hba_file="+certs+"/pg_hba.conf",
		"-c", "shared_buffers=32MB", "-c", "max_connections=20")
	process.Stdout, process.Stderr = logs, logs
	process.Env = os.Environ()
	isolateChild(process)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	ready := false
	for attempts := 0; attempts < 200 && !ready; attempts++ {
		if db, err := r.db(ctx, true); err == nil {
			db.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		b, _ := os.ReadFile(r.Config.StateDirectory + "/postgres-builder.log")
		t.Fatalf("PostgreSQL did not become ready: %s", b)
	}
	if err := r.Execute(ctx, []string{"database-init"}); err != nil {
		t.Fatal("initialize:", err)
	}
	db, err := r.db(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{`CREATE TABLE builder_workers(id TEXT PRIMARY KEY,drained BOOL NOT NULL,updated_at TIMESTAMPTZ NOT NULL)`, `CREATE TABLE build_runs(id TEXT PRIMARY KEY,state TEXT NOT NULL,builder_id TEXT NOT NULL)`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
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
