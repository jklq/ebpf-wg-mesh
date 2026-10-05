package certificates

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

type memoryStore struct {
	mu         sync.Mutex
	records    map[string]Record
	versions   map[string]Version
	challenges map[string]xds.Challenge
	accounts   map[string]Account
	pruned     []string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		records:    map[string]Record{},
		versions:   map[string]Version{},
		challenges: map[string]xds.Challenge{},
		accounts:   map[string]Account{},
	}
}

func (m *memoryStore) ListCertificates(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, record := range m.records {
		out = append(out, record)
	}
	return out, nil
}

func (m *memoryStore) CertificateRecord(_ context.Context, hostname string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[hostname]
	return record, ok, nil
}

func (m *memoryStore) SaveIssued(_ context.Context, version Version, renewAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.versions[version.Fingerprint] = version
	m.records[version.Hostname] = Record{
		Hostname: version.Hostname, Fingerprint: version.Fingerprint, NotAfter: version.NotAfter,
		RenewAt: renewAt, NextAttemptAt: renewAt,
	}
	return nil
}

func (m *memoryStore) RecordFailure(_ context.Context, hostname string, attempts int, next time.Time, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[hostname]
	record.Hostname, record.Attempts, record.NextAttemptAt, record.LastError = hostname, attempts, next, message
	m.records[hostname] = record
	return nil
}

func (m *memoryStore) CertificateVersion(_ context.Context, fingerprint string) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	version, ok := m.versions[fingerprint]
	if !ok {
		return Version{}, errors.New("not found")
	}
	return version, nil
}

func (m *memoryStore) PruneCertificates(_ context.Context, keep []string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruned = append([]string(nil), keep...)
	return nil
}

func (m *memoryStore) PutChallenge(_ context.Context, challenge xds.Challenge, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenges[challenge.Token] = challenge
	return nil
}

func (m *memoryStore) DeleteChallenge(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.challenges, token)
	return nil
}

func (m *memoryStore) ListChallenges(context.Context) ([]xds.Challenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []xds.Challenge
	for _, challenge := range m.challenges {
		out = append(out, challenge)
	}
	return out, nil
}

func (m *memoryStore) LoadACMEAccount(_ context.Context, directory string) (Account, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account, ok := m.accounts[directory]
	return account, ok, nil
}

func (m *memoryStore) SaveACMEAccount(_ context.Context, account Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[account.DirectoryURL] = account
	return nil
}

type staticHosts []Hostname

func (h staticHosts) RoutedHostnames(context.Context) ([]Hostname, error) { return h, nil }

type fakeIngress struct {
	mu        sync.Mutex
	syncs     int
	requested int
	// seen records the challenges that each synchronous publish carried.
	seen  [][]xds.Challenge
	store *memoryStore
}

func (f *fakeIngress) Sync(ctx context.Context) error {
	challenges, _ := f.store.ListChallenges(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	f.seen = append(f.seen, challenges)
	return nil
}

func (f *fakeIngress) Applied(context.Context) (int, bool, error) { return 1, true, nil }

func (f *fakeIngress) RequestSync() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested++
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) issue(t *testing.T, notBefore time.Time, lifetime time.Duration, names ...string) Issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial, DNSNames: names,
		NotBefore: notBefore, NotAfter: notBefore.Add(lifetime),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return Issued{
		ChainPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

// fakeIssuer answers one HTTP-01 challenge per order, like a CA would, and
// records whether the challenge was present when it validated.
type fakeIssuer struct {
	t   *testing.T
	ca  *testCA
	now func() time.Time
	err error

	mu        sync.Mutex
	issued    []string
	validated []bool
	store     *memoryStore
}

func (f *fakeIssuer) Issue(ctx context.Context, hostname string, solver Solver) (Issued, error) {
	if f.err != nil {
		return Issued{}, f.err
	}
	token := "token-" + hostname
	if err := solver.Present(ctx, hostname, token, token+".thumbprint"); err != nil {
		return Issued{}, err
	}
	_, present := f.store.challenges[token]
	if err := solver.CleanUp(ctx, hostname, token); err != nil {
		return Issued{}, err
	}
	f.mu.Lock()
	f.issued = append(f.issued, hostname)
	f.validated = append(f.validated, present)
	f.mu.Unlock()
	return f.ca.issue(f.t, f.now().Add(-time.Minute), 90*24*time.Hour, hostname), nil
}

type harness struct {
	service *Service
	store   *memoryStore
	ingress *fakeIngress
	issuer  *fakeIssuer
	now     *time.Time
}

func newHarness(t *testing.T, hosts []Hostname, mutate func(*Config)) *harness {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := newMemoryStore()
	ingress := &fakeIngress{store: store}
	issuer := &fakeIssuer{t: t, ca: newTestCA(t), now: clock, store: store}
	cfg := Config{Store: store, Hosts: staticHosts(hosts), Ingress: ingress, Issuer: issuer, Now: clock}
	if mutate != nil {
		mutate(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{service: service, store: store, ingress: ingress, issuer: issuer, now: &now}
}

func (h *harness) reconcile(t *testing.T) {
	t.Helper()
	var workers sync.WaitGroup
	var lastPrune time.Time
	if err := h.service.reconcile(context.Background(), make(chan struct{}, 4), &workers, &lastPrune); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
}

func TestServiceIssuesAndServesCertificate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, nil)
	if got := h.service.Status(context.Background(), "app.example.com"); got.State != StatePending {
		t.Fatalf("status before issuance = %+v, want pending", got)
	}
	h.reconcile(t)

	if !slices.Equal(h.issuer.validated, []bool{true}) {
		t.Fatalf("challenge present at validation = %v, want [true]", h.issuer.validated)
	}
	if len(h.ingress.seen) != 1 || len(h.ingress.seen[0]) != 1 || h.ingress.seen[0][0].Hostname != "app.example.com" {
		t.Fatalf("published challenges = %+v, want the app challenge before validation", h.ingress.seen)
	}
	if len(h.store.challenges) != 0 {
		t.Fatal("challenge must be removed after validation")
	}
	certs, challenges, err := h.service.IngressCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 || certs[0].Name != "app.example.com" || len(challenges) != 0 {
		t.Fatalf("served certificates = %+v challenges = %+v", certs, challenges)
	}
	pair, err := h.service.KeyPair(context.Background(), certs[0].Fingerprint)
	if err != nil || len(pair.PrivateKeyPEM) == 0 {
		t.Fatalf("key pair = %v, %v", pair, err)
	}
	status := h.service.Status(context.Background(), "app.example.com")
	if status.State != StateActive || status.ExpiresAt.IsZero() {
		t.Fatalf("status = %+v, want active with expiry", status)
	}
	if h.ingress.requested == 0 {
		t.Fatal("a new certificate must request an ingress sync")
	}

	// Not due again until the renewal time.
	h.reconcile(t)
	if len(h.issuer.issued) != 1 {
		t.Fatalf("issued %v, want one issuance before renewal time", h.issuer.issued)
	}
	*h.now = h.now.Add(61 * 24 * time.Hour)
	h.reconcile(t)
	if len(h.issuer.issued) != 2 {
		t.Fatalf("issued %v, want a renewal after two thirds of the lifetime", h.issuer.issued)
	}
}

func TestServiceStopsServingRemovedHostnames(t *testing.T) {
	t.Parallel()

	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, nil)
	h.reconcile(t)
	h.service.hosts = staticHosts(nil)
	certs, _, err := h.service.IngressCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 0 {
		t.Fatalf("certificates for an unrouted hostname = %+v", certs)
	}
}

func TestServiceBacksOffAfterFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, nil)
	h.issuer.err = errors.New("urn:ietf:params:acme:error:connection: timeout")
	h.reconcile(t)
	record := h.store.records["app.example.com"]
	if record.Attempts != 1 || !record.NextAttemptAt.Equal(h.now.Add(5*time.Minute)) {
		t.Fatalf("record = %+v, want one attempt and a retry in five minutes", record)
	}
	status := h.service.Status(context.Background(), "app.example.com")
	if status.State != StateFailed || status.Message == "" || status.RetryAt.IsZero() {
		t.Fatalf("status = %+v, want failed with message and retry", status)
	}

	h.issuer.err = nil
	h.reconcile(t)
	if len(h.issuer.issued) != 0 {
		t.Fatal("the retry delay must hold back the next attempt")
	}
	*h.now = h.now.Add(5 * time.Minute)
	h.reconcile(t)
	if len(h.issuer.issued) != 1 || h.store.records["app.example.com"].Attempts != 0 {
		t.Fatalf("issued %v record %+v, want a successful retry that resets attempts", h.issuer.issued, h.store.records["app.example.com"])
	}
}

func TestServiceWaitsForCustomHostnameDNS(t *testing.T) {
	t.Parallel()

	dnsReady := false
	h := newHarness(t, []Hostname{{Name: "www.customer.com", PlatformHostname: "jade-x.apps.example.net"}}, func(cfg *Config) {
		cfg.Verify = func(_ context.Context, hostname, platformHostname string) error {
			if hostname != "www.customer.com" || platformHostname != "jade-x.apps.example.net" {
				t.Errorf("verify(%q, %q)", hostname, platformHostname)
			}
			if !dnsReady {
				return errors.New("no CNAME")
			}
			return nil
		}
	})
	h.reconcile(t)
	if len(h.issuer.issued) != 0 || len(h.store.records) != 0 {
		t.Fatal("a hostname without DNS must not spend an issuance attempt")
	}
	dnsReady = true
	h.reconcile(t)
	if len(h.issuer.issued) != 1 {
		t.Fatalf("issued %v, want issuance once DNS points at the platform", h.issuer.issued)
	}
}

func TestPlatformCertificateCoversGeneratedHostnames(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	wildcard := ca.issue(t, time.Now().Add(-time.Hour), 90*24*time.Hour, "*.apps.example.net")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, wildcard.ChainPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, wildcard.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, []Hostname{{Name: "jade-x.apps.example.net"}, {Name: "app.example.com"}}, func(cfg *Config) {
		cfg.PlatformSuffix, cfg.PlatformCertFile, cfg.PlatformKeyFile = "apps.example.net", certFile, keyFile
		cfg.Now = time.Now
	})
	h.issuer.now = time.Now
	h.reconcile(t)
	if !slices.Equal(h.issuer.issued, []string{"app.example.com"}) {
		t.Fatalf("issued %v, want only the hostname outside the wildcard", h.issuer.issued)
	}
	if got := h.service.Status(context.Background(), "jade-x.apps.example.net"); got.State != StateActive {
		t.Fatalf("wildcard-covered status = %+v, want active", got)
	}
	certs, _, err := h.service.IngressCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cert := range certs {
		names = append(names, cert.Name)
		if _, err := h.service.KeyPair(context.Background(), cert.Fingerprint); err != nil {
			t.Fatalf("key pair for %s: %v", cert.Name, err)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"*.apps.example.net", "app.example.com"}) {
		t.Fatalf("served certificates = %v", names)
	}

	if _, err := newPlatformCertificate(certFile, keyFile, "other.example.org"); err == nil {
		t.Fatal("a wildcard for another suffix must be refused")
	}
}

func TestServiceWithoutIssuerIsUnmanaged(t *testing.T) {
	t.Parallel()

	h := newHarness(t, []Hostname{{Name: "app.example.com"}}, func(cfg *Config) { cfg.Issuer = nil })
	if h.service.Managed() {
		t.Fatal("no issuer and no platform certificate must be unmanaged")
	}
	if got := h.service.Status(context.Background(), "app.example.com"); got.State != StateUnmanaged {
		t.Fatalf("status = %+v, want unmanaged", got)
	}
	certs, challenges, err := h.service.IngressCertificates(context.Background())
	if err != nil || len(certs) != 0 || len(challenges) != 0 {
		t.Fatalf("certs = %v challenges = %v err = %v", certs, challenges, err)
	}
}

func TestRetryDelaySchedule(t *testing.T) {
	t.Parallel()

	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for i, delay := range want {
		if got := retryDelay(i+1, errors.New("x")); got != delay {
			t.Fatalf("retryDelay(%d) = %v, want %v", i+1, got, delay)
		}
	}
}

func TestParseVersionChecksHostnameAndKey(t *testing.T) {
	t.Parallel()

	ca := newTestCA(t)
	issued := ca.issue(t, time.Now(), time.Hour, "app.example.com")
	if _, err := ParseVersion("other.example.com", issued.ChainPEM, issued.KeyPEM); err == nil {
		t.Fatal("a certificate for another hostname must be refused")
	}
	other := ca.issue(t, time.Now(), time.Hour, "app.example.com")
	if _, err := ParseVersion("app.example.com", issued.ChainPEM, other.KeyPEM); err == nil {
		t.Fatal("a key that does not match the leaf must be refused")
	}
	version, err := ParseVersion("app.example.com", issued.ChainPEM, issued.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFingerprint(version); err != nil {
		t.Fatal(err)
	}
	version.Fingerprint = "00"
	if err := verifyFingerprint(version); err == nil {
		t.Fatal("a fingerprint mismatch must be detected")
	}
}
