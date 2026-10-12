package productionops

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

// fakeEnvoyAdmin keeps accepting ingress connections for a while after health
// checks fail, as a balancer does until its own checks observe the failure.
type fakeEnvoyAdmin struct {
	mu           sync.Mutex
	calls        []string
	failedAt     time.Time
	arrivals     time.Duration
	total        int64
	active       int64
	requests     int64
	healthChecks int64
	drainedAt    time.Time
}

func (f *fakeEnvoyAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/healthcheck/fail":
		f.calls = append(f.calls, "healthcheck/fail")
		f.failedAt = now
	case r.Method == http.MethodPost && r.URL.Path == "/drain_listeners" && r.URL.RawQuery == "graceful&skip_exit":
		f.calls = append(f.calls, "drain_listeners")
		f.drainedAt = now
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		if r.URL.Query().Get("format") != "json" || !strings.Contains(r.URL.Query().Get("filter"), "ingress_(https?|health)") {
			http.Error(w, "unexpected stats query", http.StatusBadRequest)
			return
		}
		if !f.failedAt.IsZero() && now.Sub(f.failedAt) < f.arrivals {
			f.total++
		}
		if !f.drainedAt.IsZero() && f.active > 0 {
			f.active--
		}
		if f.requests > 0 {
			f.requests--
		}
		fmt.Fprintf(w, `{"stats":[{"name":"http.ingress_http.downstream_cx_total","value":%d},{"name":"http.ingress_https.downstream_cx_total","value":1},{"name":"http.ingress_http.downstream_cx_active","value":%d},{"name":"http.ingress_https.downstream_cx_active","value":0},{"name":"http.ingress_http.downstream_rq_active","value":%d},{"name":"http.ingress_health.downstream_rq_total","value":%d},{"name":"http.ingress_health.downstream_cx_active","value":7}]}`, f.total, f.active, f.requests, f.healthChecks)
	default:
		http.Error(w, "unexpected admin request", http.StatusNotFound)
	}
}

func TestEnvoyStopWithdrawsBeforeDrainingAndWaitsForConnections(t *testing.T) {
	admin := &fakeEnvoyAdmin{arrivals: 300 * time.Millisecond, active: 3, healthChecks: 12}
	server := httptest.NewServer(admin)
	defer server.Close()
	e := envoyStop{admin: server.URL, withdrawalMin: 50 * time.Millisecond, withdrawalMax: 5 * time.Second, quiet: 150 * time.Millisecond, drain: 5 * time.Second, poll: 10 * time.Millisecond, client: server.Client(), replicated: true}
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	admin.mu.Lock()
	defer admin.mu.Unlock()
	if !slices.Equal(admin.calls, []string{"healthcheck/fail", "drain_listeners"}) {
		t.Fatalf("admin calls %v", admin.calls)
	}
	// New connections arrived for 300ms; listeners must not drain before the
	// balancer stopped sending them and the quiet period confirmed it.
	if waited := admin.drainedAt.Sub(admin.failedAt); waited < admin.arrivals+e.quiet-3*e.poll {
		t.Fatalf("listeners drained %v after health checks failed, while connections still arrived", waited)
	}
	if admin.active != 0 {
		t.Fatalf("stop preparation returned with %d active connections", admin.active)
	}
}

func TestEnvoyStopBoundsWithdrawalAndToleratesExitedProcess(t *testing.T) {
	admin := &fakeEnvoyAdmin{arrivals: time.Hour, healthChecks: 1}
	server := httptest.NewServer(admin)
	defer server.Close()
	e := envoyStop{admin: server.URL, withdrawalMin: 10 * time.Millisecond, withdrawalMax: 200 * time.Millisecond, quiet: time.Second, drain: time.Second, poll: 10 * time.Millisecond, client: server.Client(), replicated: true}
	start := time.Now()
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("withdrawal was not bounded: %v", elapsed)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	e.admin = closed
	if err := e.run(context.Background()); err != nil {
		t.Fatalf("stopped Envoy failed stop preparation: %v", err)
	}
	if err := prepareStop(context.Background(), []string{"envoy", "--admin", "http://10.0.0.1:19000"}); err == nil {
		t.Fatal("non-loopback admin interface accepted")
	}
}

func TestEnvoyStopMatchesWithdrawalToItsDeployment(t *testing.T) {
	// No balancer has ever checked this Envoy: failing checks would not move
	// traffic, so only the listener drain runs.
	unchecked := &fakeEnvoyAdmin{arrivals: time.Hour, active: 2}
	server := httptest.NewServer(unchecked)
	defer server.Close()
	e := envoyStop{admin: server.URL, withdrawalMin: time.Second, withdrawalMax: 2 * time.Second, quiet: time.Second, drain: time.Second, poll: 10 * time.Millisecond, client: server.Client(), replicated: true}
	start := time.Now()
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(unchecked.calls, []string{"drain_listeners"}) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("unchecked Envoy waited for a withdrawal nothing performs: %v in %v", unchecked.calls, time.Since(start))
	}
	// A sole Envoy keeps serving and its balancer has nowhere else to go: never
	// fail health checks or close listeners, only let admitted requests finish.
	sole := &fakeEnvoyAdmin{arrivals: time.Hour, active: 50, requests: 3, healthChecks: 9}
	soleServer := httptest.NewServer(sole)
	defer soleServer.Close()
	e = envoyStop{admin: soleServer.URL, withdrawalMin: time.Second, withdrawalMax: 2 * time.Second, quiet: time.Second, drain: time.Minute, poll: 10 * time.Millisecond, client: soleServer.Client()}
	start = time.Now()
	if err := e.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sole.calls) != 0 || sole.requests != 0 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("sole Envoy stop: calls %v, %d requests left, %v", sole.calls, sole.requests, time.Since(start))
	}
}

func TestStopPreparationReadsTheInstalledReplicaCount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLATFORM_CONFIG_DIR", dir)
	if !replicated() {
		t.Fatal("unknown replica count must withdraw")
	}
	for count, want := range map[string]bool{"1\n": false, "3\n": true} {
		if err := os.WriteFile(filepath.Join(dir, "replicas"), []byte(count), 0o600); err != nil {
			t.Fatal(err)
		}
		if replicated() != want {
			t.Fatalf("replicas %q: replicated = %v", count, !want)
		}
	}
	// The sole console is not marked draining: its balancer has no other target.
	t.Setenv("MAINPID", "4242")
	t.Setenv("INVOCATION_ID", "0123abcd")
	if err := os.WriteFile(filepath.Join(dir, "replicas"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "draining")
	if err := prepareStop(context.Background(), []string{"console", "--withdrawal", "1h", marker}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("sole console withdrew from its balancer")
	}
}

func TestConsoleStopNamesOnlyTheStoppingInvocation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "draining")
	t.Setenv("MAINPID", "4242")
	t.Setenv("INVOCATION_ID", "0123abcd")
	if err := prepareStop(context.Background(), []string{"console", "--withdrawal", "10ms", file}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "0123abcd\n" {
		t.Fatalf("drain marker %q: %v", b, err)
	}
	if info, _ := os.Stat(file); info.Mode().Perm()&0o077 != 0 {
		t.Fatal("drain marker is not private")
	}
	other := filepath.Join(t.TempDir(), "draining")
	t.Setenv("MAINPID", "")
	if err := prepareStop(context.Background(), []string{"console", "--withdrawal", "1h", other}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("exited console was marked draining")
	}
}

func TestEnvoyIdentityInspectionRequiresTheLoadedSerial(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := new(big.Int).SetString("0a1b2c3d4e5f", 16)
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "ingress-a"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(t.TempDir(), "client.crt")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	var loaded string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/certs" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"certificates":[{"ca_cert":[{"serial_number":"01"}],"cert_chain":[{"serial_number":%q}]}]}`, loaded)
	}))
	defer server.Close()
	loaded = "a1b2c3d4e5f0"
	if err := inspectEnvoyIdentity(context.Background(), server.URL, certFile); err == nil {
		t.Fatal("previous identity accepted as loaded")
	}
	loaded = "0a1b2c3d4e5f"
	if err := inspectEnvoyIdentity(context.Background(), server.URL, certFile); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialDigestCoversOnlyLaunchInputs(t *testing.T) {
	digest := func(role deploy.Role, files map[string][]byte) string {
		return string(credentialRuntimeEnvironment(role, files, map[string]string{"KEY": "value"}))
	}
	base := map[string][]byte{"envoy.yaml": []byte("bootstrap"), "ingress/generation-1/client.crt": []byte("one"), "client.crt": []byte("one"), "probe.json": []byte("{}")}
	renewed := map[string][]byte{"envoy.yaml": []byte("bootstrap"), "ingress/generation-2/client.crt": []byte("two"), "client.crt": []byte("two"), "probe.json": []byte(`{"url":"x"}`)}
	if digest(deploy.Envoy, base) != digest(deploy.Envoy, renewed) {
		t.Fatal("SDS identity renewal would restart Envoy")
	}
	if digest(deploy.Envoy, base) == digest(deploy.Envoy, map[string][]byte{"envoy.yaml": []byte("changed bootstrap")}) {
		t.Fatal("bootstrap change would not restart Envoy")
	}
	for _, role := range []deploy.Role{deploy.Builder, deploy.Agent, deploy.Console, deploy.ControlPlane} {
		identity := map[deploy.Role]string{deploy.Agent: "tls/client.crt"}[role]
		if identity == "" {
			identity = "client.crt"
		}
		live := !slices.Contains([]deploy.Role{deploy.Console, deploy.ControlPlane}, role)
		same := digest(role, map[string][]byte{"ca.crt": []byte("ca"), identity: []byte("one")}) == digest(role, map[string][]byte{"ca.crt": []byte("ca"), identity: []byte("two")})
		if same != live {
			t.Fatalf("%s identity renewal restart = %v, want %v", role, !same, !live)
		}
		if digest(role, map[string][]byte{"ca.crt": []byte("ca")}) == digest(role, map[string][]byte{"ca.crt": []byte("rotated")}) {
			t.Fatalf("%s trust bundle change would not restart the process", role)
		}
	}
}
