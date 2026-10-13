//go:build integration

package recovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/testdb"
)

// TestPgBackRestPointInTimeRestore exercises the whole replacement for
// CockroachDB native BACKUP/RESTORE with real binaries: WAL archiving into
// a pgBackRest repository, a full backup, a snapshot bound to a WAL
// segment, repository inspection through the provider seam, and a
// point-in-time restore that must not contain later mutations. It runs
// where PostgreSQL and pgBackRest binaries are installed
// (POSTGRES_BIN_DIR / PLATFORM_PGBACKREST_BINARY).
func TestPgBackRestPointInTimeRestore(t *testing.T) {
	binDir, ok := testdb.Binaries()
	if !ok {
		t.Skip("set POSTGRES_BIN_DIR to local PostgreSQL binaries")
	}
	backrestBinary := os.Getenv("PLATFORM_PGBACKREST_BINARY")
	if backrestBinary == "" {
		path, err := exec.LookPath("pgbackrest")
		if err != nil {
			t.Skip("pgbackrest binary not found")
		}
		backrestBinary = path
	}
	if os.Geteuid() == 0 {
		t.Skip("PostgreSQL refuses to run as root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	data := filepath.Join(root, "data")
	restored := filepath.Join(root, "restored")
	for _, dir := range []string{repo, filepath.Join(root, "spool"), filepath.Join(root, "lock"), filepath.Join(root, "log")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	port := freeTCPPort(t)
	configFile := filepath.Join(root, "pgbackrest.conf")
	configBody := fmt.Sprintf(`[global]
repo1-type=posix
repo1-path=%s
repo1-retention-full=2
archive-async=n
spool-path=%s
lock-path=%s
log-path=%s
log-level-console=warn
start-fast=y
[platform]
pg1-path=%s
pg1-user=postgres
pg1-port=%d
pg1-socket-path=%s
`, repo, filepath.Join(root, "spool"), filepath.Join(root, "lock"), filepath.Join(root, "log"), data, port, root)
	if err := os.WriteFile(configFile, []byte(configBody), 0600); err != nil {
		t.Fatal(err)
	}
	backups := platformdb.PgBackRest{Binary: backrestBinary, ConfigFile: configFile, Stanza: "platform"}
	mkRun := func(name string, args ...string) (*exec.Cmd, *os.File) {
		log, err := os.Create(filepath.Join(root, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, filepath.Join(binDir, "postgres"), args...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Env = os.Environ()
		return cmd, log
	}
	startServer := func(name, dir string, serverPort int) (*exec.Cmd, *os.File) {
		cmd, log := mkRun(name, "-D", dir,
			"-c", "listen_addresses=127.0.0.1", "-c", fmt.Sprintf("port=%d", serverPort),
			"-c", "unix_socket_directories="+root,
			"-c", "wal_level=replica", "-c", "archive_mode=on",
			"-c", fmt.Sprintf("archive_command=%s --config=%s --stanza=platform archive-push %%p", backrestBinary, configFile),
			"-c", "archive_timeout=5", "-c", "fsync=off", "-c", "shared_buffers=32MB", "-c", "max_connections=20")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, log
	}
	urlFor := func(serverPort int) string {
		return fmt.Sprintf("postgresql://postgres@127.0.0.1:%d/postgres?sslmode=disable", serverPort)
	}
	waitReady := func(serverPort int) {
		deadline := time.Now().Add(60 * time.Second)
		for {
			if db, err := platformdb.OpenURL(ctx, urlFor(serverPort), "public"); err == nil {
				var inRecovery bool
				err = db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery)
				db.Close()
				if err == nil && !inRecovery {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("server on %d did not become ready", serverPort)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}

	init := exec.CommandContext(ctx, filepath.Join(binDir, "initdb"), "-D", data, "-U", "postgres", "--auth=trust", "--no-instructions", "--data-checksums")
	init.Env = os.Environ()
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v %s", err, out)
	}
	server, serverLog := startServer("primary", data, port)
	defer func() { server.Process.Kill(); server.Wait(); serverLog.Close() }()
	waitReady(port)
	if err := backups.CreateStanza(ctx); err != nil {
		t.Fatal("stanza-create:", err)
	}
	if err := backups.Check(ctx); err != nil {
		t.Fatal("pgBackRest check:", err)
	}
	db, err := platformdb.OpenURL(ctx, urlFor(port), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE evidence(value TEXT); INSERT INTO evidence VALUES ('before')`); err != nil {
		t.Fatal(err)
	}
	if err := backups.Backup(ctx, platformdb.BackupFull); err != nil {
		t.Fatal("full backup:", err)
	}
	// Bind a snapshot moment to its WAL segment exactly as completion does.
	var snapshotTime time.Time
	var segment string
	if err := db.QueryRowContext(ctx, `SELECT transaction_timestamp(), pg_walfile_name(pg_current_wal_lsn())`).Scan(&snapshotTime, &segment); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE evidence SET value='after'`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWALArchived(ctx, db, segment); err != nil {
		t.Fatal(err)
	}
	point, err := InspectRepository(ctx, backups, "platform", "posix://"+repo, "", snapshotTime, segment)
	if err != nil {
		t.Fatal(err)
	}
	if point.BackupLabel == "" || len(point.Chain) != 1 || point.WALStop != segment {
		t.Fatal("inspected chain does not cover the snapshot", point)
	}
	// Restore from a materialized repository copy config, as production
	// restore does with frozen objects.
	restoreConfig := filepath.Join(root, "pgbackrest-restore.conf")
	if err := os.WriteFile(restoreConfig, []byte(RenderPgBackRestPosixConfig("platform", restored, repo)), 0600); err != nil {
		t.Fatal(err)
	}
	restorer := platformdb.PgBackRest{Binary: backrestBinary, ConfigFile: restoreConfig, Stanza: "platform"}
	if err := os.MkdirAll(restored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restorer.Restore(ctx, restored, point.BackupLabel, snapshotTime); err != nil {
		t.Fatal("restore:", err)
	}
	restoredPort := freeTCPPort(t)
	restoredServer, restoredLog := startServer("restored", restored, restoredPort)
	defer func() { restoredServer.Process.Kill(); restoredServer.Wait(); restoredLog.Close() }()
	waitReady(restoredPort)
	restoredDB, err := platformdb.OpenURL(ctx, urlFor(restoredPort), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	var value string
	if err := restoredDB.QueryRowContext(ctx, `SELECT value FROM evidence`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "before" {
		t.Fatalf("point-in-time restore contains the later mutation: %q", value)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
