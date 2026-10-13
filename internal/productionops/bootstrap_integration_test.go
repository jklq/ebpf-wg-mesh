//go:build integration

package productionops

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/platformdb"
	"ebof-wg-mesh/internal/recovery"
	"ebof-wg-mesh/internal/testdb"
)

// Uses the reference certificate and initializer code against a new native,
// secure PostgreSQL process. The fixture has no cloud credentials or
// infrastructure API. It runs where PostgreSQL binaries are installed
// (POSTGRES_BIN_DIR or the standard package paths).
func TestNativeSecureFreshBootstrapCredentialsAndInterruption(t *testing.T) {
	binDir, ok := testdb.Binaries()
	if !ok {
		t.Skip("set POSTGRES_BIN_DIR to local PostgreSQL binaries")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun is required to compile the console release helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := testRunner(t)
	r.Config.Database.PostgresBinary = filepath.Join(binDir, "postgres")
	r.Config.Database.InitDBBinary = filepath.Join(binDir, "initdb")
	r.Plan.Installation.Hosts[0].Network.Address = "127.0.0.6"
	r.Config.Database.Address = freeAddress(t, "127.0.0.6")
	r.Config.Console = ConsoleConfig{Schema: "console", TokenKeyFile: r.Config.StateDirectory + "/console.key", AdminBinary: r.Config.StateDirectory + "/console-admin"}
	cmd := exec.CommandContext(ctx, bun, "build", "--compile", "--target=bun-linux-x64", "tooling/production-admin.ts", "--outfile", r.Config.Console.AdminBinary)
	cmd.Dir = "../../console"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile console helper: %v %s", err, b)
	}
	rec := recovery.Config{Storage: recovery.StorageConfig{Bucket: "isolated", Prefix: "test", Region: "test", Owner: "test", CredentialsFile: r.Config.StateDirectory + "/credentials", Profile: "test", Account: "isolated-recovery", PrimaryAccount: "isolated-primary", FailureDomain: "isolated-recovery", PrimaryDomains: []string{"isolated-primary"}, WriterPrincipal: "test"}, BackupPrefix: "production/database", RecoveryKeyFile: r.Config.StateDirectory + "/recovery.key", KeyringFile: r.Config.StateDirectory + "/keyring.json"}
	if err := writePrivate(rec.RecoveryKeyFile, []byte(strings.Repeat("r", 32))); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(rec.Storage.CredentialsFile, []byte("[test]\naws_access_key_id=access\naws_secret_access_key=secret\n")); err != nil {
		t.Fatal(err)
	}
	r.Config.RecoveryConfig = r.Config.StateDirectory + "/recovery-config.json"
	if err := saveJSON(r.Config.RecoveryConfig, rec); err != nil {
		t.Fatal(err)
	}
	if err := r.databaseCredentials(ctx, false); err != nil {
		t.Fatal(err)
	}
	pl := r.Plan.Placements[0]
	host := r.Plan.Installation.Hosts[0]
	remote := r.Remote.(isolatedRemote)
	certs := remote.path(host, cfgDir(r.Plan, pl)+"/certs")
	data := remote.path(host, dataDir(r.Plan, pl)+"/data")
	logs, err := os.Create(r.Config.StateDirectory + "/postgres.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	_, port, _ := net.SplitHostPort(r.Config.Database.Address)
	process := exec.CommandContext(ctx, r.Config.Database.PostgresBinary,
		"-D", data,
		"-c", "listen_addresses=127.0.0.6", "-c", "port="+port, "-c", "unix_socket_directories="+r.Config.StateDirectory,
		"-c", "ssl=on",
		"-c", "ssl_cert_file="+certs+"/node.crt",
		"-c", "ssl_key_file="+certs+"/node.key",
		"-c", "ssl_ca_file="+certs+"/ca.crt",
		"-c", "hba_file="+certs+"/pg_hba.conf",
		"-c", "wal_level=replica", "-c", "archive_mode=on", "-c", "archive_command=true",
		"-c", "shared_buffers=32MB", "-c", "max_connections=20")
	process.Stdout, process.Stderr = logs, logs
	process.Env = os.Environ()
	isolateChild(process)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	// Wait for the listener on the maintenance database, then initialize
	// exactly once through the operations hook.
	raw, err := os.ReadFile(r.Config.Database.URLFile)
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := platformdb.ReplaceDatabase(strings.TrimSpace(string(raw)), "postgres")
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	for attempts := 0; attempts < 200 && !ready; attempts++ {
		if db, err := platformdb.OpenURL(ctx, maintenance, platformdb.SchemaPlatform); err == nil {
			db.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		b, _ := os.ReadFile(r.Config.StateDirectory + "/postgres.log")
		t.Fatalf("PostgreSQL did not become ready:\n%s", b)
	}
	if err := r.Execute(ctx, []string{"database-init"}); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := r.Execute(ctx, []string{"database-init"}); err != nil {
		t.Fatal("initialize retry", err)
	}
	if err := r.prepareInitialKeys(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := r.db(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// PostgreSQL bootstrap is the implementation used after independently protecting
	// keys; the separate storage suite verifies that protection gate.
	if err := controlplane.BootstrapInstallation(ctx, db, rec.KeyringFile); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recovery_runtime_authority(singleton,installation,generation,paused) VALUES(TRUE,$1,$2,FALSE)`, r.Plan.Installation.ID, r.Plan.Generation); err != nil {
		t.Fatal(err)
	}
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()
	if err := r.exportSigning(ctx, signing); err != nil {
		t.Fatal(err)
	}
	if err := r.consoleAdmin(ctx, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyBootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// An upgrade verifies the provisioned PostgreSQL keyring, without calling the
	// fresh bootstrap generator or replacing any encryption material.
	keyringBefore := mustRead(t, rec.KeyringFile)
	consoleBefore := mustRead(t, r.Config.Console.TokenKeyFile)
	r.Plan.Previous = &deploy.AppliedDeployment{Installation: r.Plan.Installation, Release: r.Plan.Release, Placements: r.Plan.Placements}
	if err := r.prepareInitialKeys(ctx); err != nil {
		t.Fatal("PostgreSQL upgrade key admission", err)
	}
	if string(mustRead(t, rec.KeyringFile)) != string(keyringBefore) || string(mustRead(t, r.Config.Console.TokenKeyFile)) != string(consoleBefore) {
		t.Fatal("upgrade changed existing encryption or console keys")
	}
	r.Plan.Previous = nil
	admitted, err := r.admitMaintenancePlan(ctx, db)
	if err != nil {
		t.Fatal("PostgreSQL scheduled plan admission", err)
	}
	admitted()
	if err := r.verifyMaintenancePlan(ctx, db); err != nil {
		t.Fatal("PostgreSQL scheduled plan observation", err)
	}
	first, err := r.acquireRenewal(ctx, db)
	if err != nil {
		t.Fatal("PostgreSQL renewal ownership", err)
	}
	if _, err := r.acquireRenewal(ctx, db); err == nil {
		t.Fatal("concurrent PostgreSQL issuer acquired active ownership")
	}
	if _, err := db.ExecContext(ctx, `UPDATE recovery.credential_renewal SET expires_at=statement_timestamp()-INTERVAL '1 minute'`); err != nil {
		t.Fatal(err)
	}
	second, err := r.acquireRenewal(ctx, db)
	if err != nil {
		t.Fatal("PostgreSQL renewal interruption ownership recovery", err)
	}
	first()
	var owners int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recovery.credential_renewal`).Scan(&owners); err != nil || owners != 1 {
		t.Fatal("old issuer released replacement ownership", err)
	}
	second()
	// Automatic placement changes retain the generation. A lost completer must
	// still be unable to renew or publish using the prior plan when it returns.
	stale := *r
	backupOwner, err := r.acquireNativeLease(ctx, db, "backup_completion", true, true)
	if err != nil {
		t.Fatal("PostgreSQL independent backup ownership", err)
	}
	r.Plan.ID = "replacement-plan"
	if release, err := r.admitMaintenancePlan(ctx, db); err == nil {
		release()
		t.Fatal("schedule replacement interrupted an owned PostgreSQL backup")
	}
	if err := stale.verifyMaintenancePlan(ctx, db); err != nil {
		t.Fatal("failed schedule replacement changed actual plan admission", err)
	}
	backupOwner()
	admitted, err = r.admitMaintenancePlan(ctx, db)
	if err != nil {
		t.Fatal("PostgreSQL replacement plan admission", err)
	}
	admitted()
	if release, err := stale.acquireRenewal(ctx, db); err == nil {
		release()
		t.Fatal("old plan renewed after actual placement admission changed")
	}
	if release, err := stale.acquireNativeLease(ctx, db, "backup_completion", true, true); err == nil {
		release()
		t.Fatal("old plan published after actual placement admission changed")
	}
	backupOwner, err = r.acquireNativeLease(ctx, db, "backup_completion", true, true)
	if err != nil {
		t.Fatal("PostgreSQL admitted replacement backup ownership", err)
	}
	backupOwner()
	// No service process is fabricated here: only DB/bootstrap roles are declared.
	// Pause and failed resume must leave actual SQL authority and scheduling paused.
	r.Plan.Placements = nil
	if err := r.Execute(ctx, []string{"quiesce"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, []string{"quiesce"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Execute(ctx, []string{"resume"}); err == nil {
		t.Fatal("missing production endpoints resumed automation")
	}
	var paused, scheduler bool
	if err := db.QueryRowContext(ctx, `SELECT paused FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT paused FROM build_scheduler_control WHERE id=TRUE`).Scan(&scheduler); err != nil {
		t.Fatal(err)
	}
	if _, err := r.acquireRenewal(ctx, db); err == nil {
		t.Fatal("paused PostgreSQL authority admitted credential maintenance")
	}
	if !paused || !scheduler {
		t.Fatal("failed verification resumed PostgreSQL authority")
	}
	// Provision an actual component SQL client and prove its certificate can read
	// the schema. An unrelated leaf signed by another CA must fail authentication.
	pl = deploy.Placement{Role: deploy.ControlPlane, Instance: "controlplane-a", Host: "a"}
	files := map[string][]byte{}
	if err := r.databaseClient(ctx, pl, db, files, map[string]string{}, false); err != nil {
		t.Fatal(err)
	}
	clientCert := r.databasePKI() + "/component_controlplane_a.json"
	var certificate certificateBundle
	if err := privateJSON(clientCert, &certificate); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"component.crt": certificate.Certificate, "component.key": certificate.Key} {
		if err := writePrivate(r.Config.StateDirectory+"/"+name, b); err != nil {
			t.Fatal(err)
		}
	}
	u := strings.Replace(string(mustRead(t, r.Config.Database.URLFile)), "root@", "component_controlplane_a@", 1)
	u = strings.ReplaceAll(u, "client.root.crt", "../../component.crt")
	u = strings.ReplaceAll(u, "client.root.key", "../../component.key")
	client, err := platformdb.OpenURL(ctx, u, platformdb.SchemaPlatform)
	if err != nil {
		t.Fatal("component client TLS", err)
	}
	defer client.Close()
	clientURL := r.Config.StateDirectory + "/component-url"
	if err := writePrivate(clientURL, []byte(u)); err != nil {
		t.Fatal(err)
	}
	if err := inspectSQLClient(ctx, clientURL, "platform"); err != nil {
		t.Fatal("actual component table and schema permissions", err)
	}
	if _, err := db.ExecContext(ctx, "REVOKE USAGE ON SCHEMA platform FROM component_controlplane_a"); err != nil {
		t.Fatal(err)
	}
	if err := inspectSQLClient(ctx, clientURL, "platform"); err == nil {
		t.Fatal("table grants hid a missing schema usage grant")
	}
	if err := r.databaseClient(ctx, pl, db, files, map[string]string{}, false); err != nil {
		t.Fatal("idempotent component permissions repair", err)
	}
	if err := inspectSQLClient(ctx, clientURL, "platform"); err != nil {
		t.Fatal("repaired component permissions", err)
	}
	r.Plan.Placements = []deploy.Placement{{Role: deploy.Agent, Instance: "enrolled-a", Host: "a"}, {Role: deploy.Agent, Instance: "enrolled-b", Host: "a"}}
	tokens, err := r.agentBootstrapTokens(ctx, signing)
	if err != nil || tokens["enrolled-a"] == tokens["enrolled-b"] {
		t.Fatal("agent tokens are not individually bound", err)
	}
	material, err := identity.IssueClientCertificate(ctx, signing, identity.CallerAgent, "enrolled-a", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, verify := range []bool{false, true, false, true} {
		if err := recordAgentEnrollment(ctx, db, "enrolled-a", tokens["enrolled-a"], material.CertPEM, verify); err != nil {
			t.Fatal("PostgreSQL consumed enrollment and retry", err)
		}
	}
	if err := recordAgentEnrollment(ctx, db, "enrolled-b", tokens["enrolled-a"], material.CertPEM, true); err == nil {
		t.Fatal("another agent was accepted under the consumed enrollment token")
	}
	r.Plan.Generation = "replacement-authority"
	newTokens, err := r.agentBootstrapTokens(ctx, signing)
	if err != nil || tokens["enrolled-a"] == newTokens["enrolled-a"] {
		t.Fatal("new authority retained the old bootstrap token", err)
	}
	r.Plan.Generation = "initial"
	r.Plan.Placements = nil
	r.Plan.Placements = []deploy.Placement{{Role: deploy.Database, Instance: "db-a", Host: "a"}}
	if _, err := db.ExecContext(ctx, `SELECT pg_switch_wal()`); err != nil {
		t.Fatal(err)
	}
	var status deploy.DatabaseStatus
	for attempts := 0; attempts < 100; attempts++ {
		var err error
		if status, err = r.databaseStatus(ctx); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || len(status.Live) != 1 || status.Live[0] != host.ID || status.Versions[host.ID] == "" {
		t.Fatal("actual primary not observed live on its custom port", status, err)
	}
	testNativeWorkQuarantine(t, ctx, r, db)
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func freeAddress(t *testing.T, host string) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(address)
}
