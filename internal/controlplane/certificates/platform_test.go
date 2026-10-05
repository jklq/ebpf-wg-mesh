package certificates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
)

func writePlatformPair(t *testing.T, dir string, issued Issued) (string, string) {
	t.Helper()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, data := range map[string][]byte{cert: issued.ChainPEM, key: issued.KeyPEM} {
		if err := os.WriteFile(path+".new", data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	return cert, key
}

func TestGeneratedHostnamesNeverOrderIndividualCertificates(t *testing.T) {
	t.Parallel()
	for _, withWildcard := range []bool{false, true} {
		t.Run(fmt.Sprintf("wildcard=%t", withWildcard), func(t *testing.T) {
			dir := t.TempDir()
			ca := newTestCA(t)
			h := newHarness(t, nil, func(cfg *Config) {
				cfg.PlatformSuffix = "apps.example.net"
				cfg.Now = time.Now
				if withWildcard {
					cfg.PlatformCertFile, cfg.PlatformKeyFile = writePlatformPair(t, dir, ca.issue(t, time.Now().Add(-time.Hour), 90*24*time.Hour, "*.apps.example.net"))
				}
			})
			h.issuer.now = time.Now
			for count := 1; count <= 100; count++ {
				var hosts []Hostname
				for i := 0; i < count; i++ {
					hosts = append(hosts, Hostname{Name: fmt.Sprintf("service-%d.apps.example.net", i), PlatformGenerated: true})
				}
				h.service.hosts = staticHosts(hosts)
				h.reconcile(t)
			}
			if len(h.issuer.orders) != 0 || len(h.store.records) != 0 || len(h.store.challenges) != 0 {
				t.Fatalf("generated hosts created orders or certificate state: orders=%v records=%v challenges=%v", h.issuer.orders, h.store.records, h.store.challenges)
			}
			// A generated flag also excludes names outside the current suffix.
			h.service.hosts = staticHosts([]Hostname{{Name: "old.generated.example.org", PlatformGenerated: true}})
			h.reconcile(t)
			if len(h.issuer.orders) != 0 {
				t.Fatal("a generated hostname fell back after its suffix changed")
			}
		})
	}
}

func TestGeneratedHostnameDoesNotServeAnIndividualCertificateFallback(t *testing.T) {
	t.Parallel()
	name := "service.apps.example.net"
	h := newHarness(t, []Hostname{{Name: name, PlatformGenerated: true}}, func(cfg *Config) { cfg.PlatformSuffix = "apps.example.net" })
	issued := h.issuer.ca.issue(t, h.now.Add(-time.Hour), 90*24*time.Hour, name)
	if err := h.service.save(context.Background(), name, issued); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutChallenge(context.Background(), xds.Challenge{Hostname: name, Token: "old-challenge"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	certificates, challenges, err := h.service.IngressCertificates(context.Background())
	if err != nil || len(certificates) != 0 || len(challenges) != 0 {
		t.Fatalf("individual fallback = %v, %v, %v", certificates, challenges, err)
	}
	if status := h.service.Status(context.Background(), name); status.State != StateFailed {
		t.Fatalf("missing wildcard status = %+v", status)
	}
}

func TestPlatformCertificateRequiresExactValidWildcard(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{RequirePlatformCertificate: true, PlatformSuffix: "apps.example.net"}); err == nil {
		t.Fatal("production accepted absent wildcard files")
	}
	ca := newTestCA(t)
	for _, tc := range []struct {
		name, san string
		notBefore time.Time
		lifetime  time.Duration
	}{
		{"individual probe", "probe.apps.example.net", time.Now().Add(-time.Hour), 24 * time.Hour},
		{"wrong wildcard", "*.example.net", time.Now().Add(-time.Hour), 24 * time.Hour},
		{"expired", "*.apps.example.net", time.Now().Add(-48 * time.Hour), 24 * time.Hour},
		{"future", "*.apps.example.net", time.Now().Add(time.Hour), 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, key := writePlatformPair(t, t.TempDir(), ca.issue(t, tc.notBefore, tc.lifetime, tc.san))
			if _, err := newPlatformCertificate(cert, key, "apps.example.net"); err == nil {
				t.Fatal("accepted invalid platform wildcard")
			}
		})
	}
}

func TestPlatformFileRenewalReloadsAndPreservesPublishedVersions(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	dir := t.TempDir()
	first := ca.issue(t, time.Now().Add(-time.Hour), 24*time.Hour, "*.apps.example.net")
	cert, key := writePlatformPair(t, dir, first)
	h := newHarness(t, []Hostname{{Name: "service.apps.example.net", PlatformGenerated: true}}, func(cfg *Config) {
		cfg.PlatformSuffix = "apps.example.net"
		cfg.PlatformCertFile = cert
		cfg.PlatformKeyFile = key
		cfg.Now = time.Now
	})
	old, _, err := h.service.IngressCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second := ca.issue(t, time.Now().Add(-time.Minute), 48*time.Hour, "*.apps.example.net")
	writePlatformPair(t, dir, second)
	renewed, _, err := h.service.IngressCertificates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(renewed) != 1 || renewed[0].Fingerprint == old[0].Fingerprint {
		t.Fatal("renewal did not reload the shared wildcard")
	}
	for _, fingerprint := range []string{old[0].Fingerprint, renewed[0].Fingerprint} {
		if _, err := h.service.KeyPair(context.Background(), fingerprint); err != nil {
			t.Fatalf("published version %s lost its key pair: %v", fingerprint, err)
		}
	}
	// An incomplete replacement retains the last good certificate instead of ordering ACME.
	writePlatformPair(t, dir, Issued{ChainPEM: second.ChainPEM, KeyPEM: first.KeyPEM})
	retained, _, err := h.service.IngressCertificates(context.Background())
	if err != nil || retained[0].Fingerprint != renewed[0].Fingerprint {
		t.Fatalf("invalid renewal = %v, %v", retained, err)
	}
	h.reconcile(t)
	if len(h.issuer.orders) != 0 {
		t.Fatal("renewal caused individual orders")
	}
	// Expiry must fail publication closed, preserving HTTPS in the last good snapshot.
	h.service.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	if _, _, err := h.service.IngressCertificates(context.Background()); err == nil {
		t.Fatal("expired wildcard silently removed HTTPS from publication")
	}
	if got := h.service.Status(context.Background(), "service.apps.example.net"); got.State != StateFailed {
		t.Fatalf("expired wildcard status = %+v", got)
	}
	h.reconcile(t)
	if len(h.issuer.orders) != 0 {
		t.Fatal("expiry caused individual orders")
	}
}
