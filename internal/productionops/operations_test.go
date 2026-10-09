package productionops

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/reconciliation"
	"github.com/jackc/pgx/v5"
)

// Executes generated scripts against isolated directories. It never fabricates
// a certificate, checksum, health result or backup receipt.
type isolatedRemote struct{ Root string }

func (s isolatedRemote) path(h deploy.Host, path string) string {
	return strings.NewReplacer("/etc/ebpf-wg-mesh/", s.Root+"/"+h.ID+"/etc/", "/var/lib/ebpf-wg-mesh/", s.Root+"/"+h.ID+"/state/").Replace(path)
}
func (s isolatedRemote) Run(ctx context.Context, i deploy.Installation, h deploy.Host, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-eu", "-c", portableHostScript(s.path(h, script)))
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("isolated script failed: %w", err)
	}
	return b, nil
}
func (s isolatedRemote) Upload(ctx context.Context, i deploy.Installation, h deploy.Host, source, target, digest string) error {
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("upload digest mismatch")
	}
	return writePrivate(s.path(h, target), b)
}
func testRunner(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	i := deploy.Installation{ID: "isolated", Providers: map[string]deploy.Provider{"linux": {Kind: "linux"}}, Hosts: []deploy.Host{{ID: "a", Binding: deploy.Binding{Provider: "linux", ServerID: "a"}, Network: deploy.Network{Address: "127.0.0.1"}}}, Components: map[deploy.Role]deploy.Component{}}
	return &Runner{Plan: deploy.Plan{ID: "plan", Generation: "initial", Installation: i, Release: deploy.Release{ID: "r43", Schema: 43, ConsoleSchema: 3}, Placements: []deploy.Placement{{Role: deploy.Database, Host: "a", Instance: "db-a"}}}, Config: Config{StateDirectory: dir, Database: DatabaseConfig{CertificateDirectory: dir + "/pki", URLFile: dir + "/database-url", Address: "127.0.0.1:26257", Name: "platform"}, Probes: map[deploy.Role]Probe{deploy.Database: {URL: "https://127.0.0.1:8080/health", Status: 200}}}, Remote: isolatedRemote{Root: dir + "/hosts"}, Adapter: deploy.NewAdapter}
}
func TestDatabaseCertificatesProvisionedBeforeStartupAndIdempotent(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()
	if _, err := r.Verify(ctx, []string{"database-credentials"}); err == nil {
		t.Fatal("missing certificates were accepted")
	}
	if err := r.Execute(ctx, []string{"database-credentials"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, []string{"database-credentials"}); err != nil {
		t.Fatal(err)
	}
	path := r.Config.StateDirectory + "/hosts/a/etc/isolated/db-a/certs/node.key"
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Execute(ctx, []string{"database-credentials"}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(first) != string(again) {
		t.Fatal("retry changed the node identity")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, []string{"database-credentials"}); err == nil {
		t.Fatal("attempt history hid changed certificate effects")
	}
	if err := r.Execute(ctx, []string{"database-credentials"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, []string{"database-credentials"}); err != nil {
		t.Fatal(err)
	}
}
func TestTLSReadinessAndAuthorizationInspectActualResponses(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/protected" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	leaf := server.Certificate()
	ca := filepath.Join(t.TempDir(), "root.crt")
	if err := os.WriteFile(ca, pemCertificate(leaf.Raw), 0600); err != nil {
		t.Fatal(err)
	}
	p := Probe{URL: server.URL + "/readyz", CAFile: ca, Status: 200, BodyContains: `"ready"`, AuthorizationURL: server.URL + "/protected", UnauthorizedStatus: 401}
	if _, err := probeHTTP(context.Background(), p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := probeHTTP(context.Background(), p, true); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if _, err := probeHTTP(context.Background(), p, false); err == nil {
		t.Fatal("failed readiness passed")
	}
	p.ServerName = "wrong-service.invalid"
	if _, err := probeHTTP(context.Background(), p, true); err == nil {
		t.Fatal("wrong TLS service identity passed")
	}
	p.ServerName = "example.com"
	if _, err := probeHTTP(context.Background(), p, true); err != nil {
		t.Fatal("selected certificate DNS identity was not verified", err)
	}
	p.ServerName = ""
	p.CAFile = ""
	if _, err := probeHTTP(context.Background(), p, true); err == nil {
		t.Fatal("untrusted endpoint passed")
	}
}
func TestCertificateIdentityAndIndependentFreshRestoreModes(t *testing.T) {
	ca, err := certificate("ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.json")
	material, err := loadCertificate(path, "node", &ca, []string{"127.0.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(material.Certificate, material.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(path, "node", &ca, []string{"127.0.0.2"}, true); err == nil {
		t.Fatal("wrong host identity accepted")
	}
	r := testRunner(t)
	r.Plan.Recovery = true
	if err := r.Execute(context.Background(), []string{"database-init"}); err == nil {
		t.Fatal("restore used the fresh schema initializer")
	}
	r.Plan.Recovery = false
	if err := r.Execute(context.Background(), []string{"recovery-database"}); err == nil {
		t.Fatal("fresh installation used restore mode")
	}
}
func TestFencingUnreachableImportedHostFailsClosed(t *testing.T) {
	r := testRunner(t)
	r.Plan.Recovery = true
	r.Plan.Previous = &deploy.AppliedDeployment{Installation: r.Plan.Installation, Release: r.Plan.Release, Placements: r.Plan.Placements}
	// No systemd manager or service exists in this isolated filesystem. An attempt
	// to stop it cannot stand in for the missing observation.
	if err := r.fence(context.Background(), true); err == nil {
		t.Fatal("unknown imported host state was accepted as fenced")
	}
}
func TestRunPrivateProbeAndCLIOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"actual":true}`)) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "probe.json")
	if err := saveJSON(path, Probe{URL: server.URL, Status: 200}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Run(context.Background(), []string{"probe", path}, &out); err != nil {
		t.Fatal(err)
	}
	var observed map[string]bool
	if err := json.Unmarshal([]byte(out.String()), &observed); err != nil || !observed["actual"] {
		t.Fatal("probe omitted the actual response")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"probe", path}, &out); err == nil {
		t.Fatal("public private configuration was accepted")
	}
}

func pemCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestReferenceServiceSelectionsValidateBeforeExpansion(t *testing.T) {
	// Operators select service addresses using the documented host substitution.
	// The complete reference configuration must load before any certificates or
	// native processes are created; expanded probes still enforce real TLS.
	b, err := os.ReadFile("../../infra/production/operations/config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := cfg.Probes[deploy.ControlPlane]
	invalid.AuthorizationURL = "http://{address}:9443/platform.v1.PlatformService/ListProjects"
	if err = invalid.Validate(); err == nil {
		t.Fatal("unencrypted host authorization inspection accepted")
	}
}

func TestDatabaseURLRetainsVerifiedIPv6Failover(t *testing.T) {
	ca, err := certificate("sql-test-ca", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := certificate("root", &ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, b := range map[string][]byte{"ca": ca.Certificate, "cert": leaf.Certificate, "key": leaf.Key} {
		if err = writePrivate(filepath.Join(dir, name), b); err != nil {
			t.Fatal(err)
		}
	}
	u, err := verifiedDatabaseURL("root", "[fd42::a]:26257,[fd42::b]:26258,sql.example.invalid:26259", "platform", dir+"/ca", dir+"/cert", dir+"/key")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u.String())
	if err != nil {
		t.Fatal("operator URL cannot be inspected", err)
	}
	cfg, err := pgx.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "fd42::a" || cfg.Port != 26257 || cfg.TLSConfig.ServerName != "fd42::a" || cfg.TLSConfig.InsecureSkipVerify || len(cfg.Fallbacks) != 2 {
		t.Fatalf("primary SQL/TLS failover: %+v", cfg.Config)
	}
	for n, want := range []string{"fd42::b", "sql.example.invalid"} {
		if cfg.Fallbacks[n].Host != want || cfg.Fallbacks[n].TLSConfig.ServerName != want || cfg.Fallbacks[n].TLSConfig.InsecureSkipVerify {
			t.Fatal("fallback weakened hostname verification", n)
		}
	}
	selectDatabaseHost(parsed, "[fd42::b]:26258")
	cfg, err = pgx.ParseConfig(parsed.String())
	if err != nil || cfg.Host != "fd42::b" || len(cfg.Fallbacks) != 0 {
		t.Fatal("console/native selected peer was overridden", err)
	}
}

// These fixtures execute Linux-host scripts on the test machine. Darwin ships
// shasum and BSD mv, so select their equivalent checksum and symlink semantics.
func portableHostScript(script string) string {
	if runtime.GOOS == "darwin" {
		return strings.NewReplacer("sha256sum", "shasum -a 256", "mv -Tf", "mv -hf").Replace(script)
	}
	return script
}

func TestPausedParticipantsIncludeCurrentAndPriorDistinctInstances(t *testing.T) {
	r := testRunner(t)
	old := deploy.Placement{Role: deploy.Agent, Host: "a", Instance: "old-agent"}
	shared := deploy.Placement{Role: deploy.ControlPlane, Host: "a", Instance: "shared-core"}
	current := deploy.Placement{Role: deploy.Agent, Host: "a", Instance: "new-agent"}
	r.Plan.Placements = []deploy.Placement{shared, current}
	r.Plan.Previous = &deploy.AppliedDeployment{Installation: r.Plan.Installation, Release: r.Plan.Release, Placements: []deploy.Placement{old, shared}}
	plans := r.participantPlans(true)
	seen := map[string]int{}
	for _, p := range plans {
		for _, pl := range p.Placements {
			seen[pl.Instance]++
		}
	}
	if len(seen) != 3 || seen[old.Instance] != 1 || seen[shared.Instance] != 1 || seen[current.Instance] != 1 {
		t.Fatal("pause omitted a current or prior participant", seen)
	}
	for _, p := range r.participantPlans(false) {
		for _, pl := range p.Placements {
			if pl.Instance == old.Instance {
				t.Fatal("resume included a prior participant")
			}
		}
	}
}

func TestCredentialIssuancePreservesLifecycleAdmission(t *testing.T) {
	r := testRunner(t)
	pl := deploy.Placement{Role: deploy.Agent, Host: "a", Instance: "agent-a"}
	wanted := reconciliation.Authority{InstallationID: r.Plan.Installation.ID, Generation: r.Plan.Generation, ClusterID: "current-ca", Paused: true}
	ctx := context.Background()
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, true); err == nil {
		t.Fatal("missing admission was verified")
	}
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, false); err != nil {
		t.Fatal(err)
	}
	path := r.Remote.(isolatedRemote).path(r.Plan.Installation.Hosts[0], cfgDir(r.Plan, pl)+"/authority.json")
	approved := wanted
	approved.Checkpoints = true
	encoded, _ := json.Marshal(approved)
	if err := writePrivate(path, encoded); err != nil {
		t.Fatal(err)
	}
	for _, verify := range []bool{false, true} {
		if err := r.ensureAdmissionIdentity(ctx, pl, wanted, verify); err != nil {
			t.Fatal("issuance reconstructed the approved mode", err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != string(encoded) {
			t.Fatal("issuance replaced lifecycle admission", err)
		}
	}
	wanted.Paused = false
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, false); err == nil {
		t.Fatal("renewal silently changed a mismatched pause")
	}
	approved.Paused = false
	encoded, _ = json.Marshal(approved)
	if err := writePrivate(path, encoded); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, true); err != nil {
		t.Fatal("resumed approved admission was rejected", err)
	}
	wanted.ClusterID = "different-ca"
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, false); err == nil {
		t.Fatal("issuance replaced a different admission identity")
	}
	pl.Role, pl.Instance = deploy.Envoy, "envoy-a"
	if err := r.ensureAdmissionIdentity(ctx, pl, wanted, false); err != nil {
		t.Fatal(err)
	}
	path = r.Remote.(isolatedRemote).path(r.Plan.Installation.Hosts[0], cfgDir(r.Plan, pl)+"/authority.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a component without admission received an authority file", err)
	}
}
