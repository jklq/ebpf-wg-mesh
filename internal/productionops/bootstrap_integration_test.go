//go:build integration

package productionops

import (
	"context"
	"database/sql"
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
	"ebof-wg-mesh/internal/recovery"
)

// Uses the reference certificate and initializer code against a new native,
// secure process. The fixture has no cloud credentials or infrastructure API.
func TestNativeSecureFreshBootstrapCredentialsAndInterruption(t *testing.T) {
	binary := os.Getenv("COCKROACH_BINARY")
	if binary == "" {
		binary = "/tmp/cockroach-v26.1.0"
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skip("set COCKROACH_BINARY to the pinned native executable")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun is required to compile the console release helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := testRunner(t)
	r.Config.Database.Binary = binary
	r.Plan.Installation.Hosts[0].Network.Address = "127.0.0.6"
	r.Config.Database.Address = freeAddress(t, "127.0.0.6")
	httpAddress := freeAddress(t, "127.0.0.6")
	r.Config.Console = ConsoleConfig{Schema: "dashboard", TokenKeyFile: r.Config.StateDirectory + "/console.key", AdminBinary: r.Config.StateDirectory + "/console-admin"}
	cmd := exec.CommandContext(ctx, bun, "build", "--compile", "--target=bun-linux-x64", "tooling/production-admin.ts", "--outfile", r.Config.Console.AdminBinary)
	cmd.Dir = "../../console"
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile console helper: %v %s", err, b)
	}
	rec := recovery.Config{Storage: recovery.StorageConfig{Bucket: "isolated", Prefix: "test", Region: "test", Owner: "test", CredentialsFile: r.Config.StateDirectory + "/credentials", Profile: "test", Account: "isolated-recovery", PrimaryAccount: "isolated-primary", FailureDomain: "isolated-recovery", PrimaryDomains: []string{"isolated-primary"}, WriterPrincipal: "test"}, RecoveryKeyFile: r.Config.StateDirectory + "/recovery.key", KeyringFile: r.Config.StateDirectory + "/keyring.json"}
	if err := writePrivate(rec.RecoveryKeyFile, []byte(strings.Repeat("r", 32))); err != nil {
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
	logs, err := os.Create(r.Config.StateDirectory + "/cockroach.log")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	process := exec.CommandContext(ctx, binary, "start", "--certs-dir="+certs, "--store="+r.Config.StateDirectory+"/store", "--listen-addr="+r.Config.Database.Address, "--advertise-addr="+r.Config.Database.Address, "--http-addr="+httpAddress, "--join="+r.Config.Database.Address, "--cache=128MiB", "--max-sql-memory=128MiB", "--external-io-dir="+r.Config.StateDirectory+"/external")
	process.Stdout, process.Stderr = logs, logs
	isolateChild(process)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	// Wait for the native listener, then initialize exactly once through the tool.
	for attempts := 0; attempts < 80; attempts++ {
		probe := Probe{URL: "https://" + httpAddress + "/health", CAFile: r.databasePKI() + "/ca.crt", Status: 200}
		if _, err := probeHTTP(ctx, probe, false); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := r.Execute(ctx, []string{"database-init"}); err != nil {
		b, _ := os.ReadFile(r.Config.StateDirectory + "/" + r.Plan.ID + "/native-init.log")
		t.Fatalf("initialize: %v\n%s", err, b)
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
	// Native bootstrap is the implementation used after independently protecting
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
	if !paused || !scheduler {
		t.Fatal("failed verification resumed native authority")
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
	client, err := sql.Open("pgx", u)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.PingContext(ctx); err != nil {
		t.Fatal("component client TLS", err)
	}
	clientURL := r.Config.StateDirectory + "/component-url"
	if err := writePrivate(clientURL, []byte(u)); err != nil {
		t.Fatal(err)
	}
	if err := inspectSQLClient(ctx, clientURL, r.Config.Console.Schema); err != nil {
		t.Fatal("actual component table and schema permissions", err)
	}
	if _, err := db.ExecContext(ctx, "REVOKE USAGE ON SCHEMA "+r.Config.Console.Schema+" FROM component_controlplane_a"); err != nil {
		t.Fatal(err)
	}
	if err := inspectSQLClient(ctx, clientURL, r.Config.Console.Schema); err == nil {
		t.Fatal("table grants hid a missing schema usage grant")
	}
	if err := r.databaseClient(ctx, pl, db, files, map[string]string{}, false); err != nil {
		t.Fatal("idempotent component permissions repair", err)
	}
	if err := inspectSQLClient(ctx, clientURL, r.Config.Console.Schema); err != nil {
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
			t.Fatal("native consumed enrollment and retry", err)
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
	if node, err := databaseNodeID(ctx, db, host.Network.Address); err != nil || node <= 0 {
		t.Fatal("actual native node with a custom advertised port", node, err)
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
