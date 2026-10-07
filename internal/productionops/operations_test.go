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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/deploy"
)

// Executes generated scripts against isolated directories. It never fabricates
// a certificate, checksum, health result or backup receipt.
type isolatedRemote struct{ Root string }

func (s isolatedRemote) path(h deploy.Host, path string) string {
	return strings.NewReplacer("/etc/ebpf-wg-mesh/", s.Root+"/"+h.ID+"/etc/", "/var/lib/ebpf-wg-mesh/", s.Root+"/"+h.ID+"/state/").Replace(path)
}
func (s isolatedRemote) Run(ctx context.Context, i deploy.Installation, h deploy.Host, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-eu", "-c", s.path(h, script))
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
