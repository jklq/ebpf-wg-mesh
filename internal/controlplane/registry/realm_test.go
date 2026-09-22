package registry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubLookup replaces the realm host resolver for one test and restores
// it afterwards. Realm tests stay offline this way.
func stubLookup(t *testing.T, ip string) {
	t.Helper()
	original := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	t.Cleanup(func() { lookupIPAddr = original })
}

func TestTokenRealmURLConfinesRealmToRegistrySite(t *testing.T) {
	stubLookup(t, "203.0.113.10")
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		realm        string
		registryHost string
		allowed      []string
		wantErr      bool
	}{
		{name: "same host https", realm: "https://ghcr.io/token", registryHost: "ghcr.io"},
		{name: "docker hub auth sibling", realm: "https://auth.docker.io/token", registryHost: "docker.io"},
		{name: "distribution endpoint sibling", realm: "https://auth.docker.io/token", registryHost: "registry-1.docker.io"},
		{name: "http loopback same authority", realm: "http://127.0.0.1:5000/token", registryHost: "127.0.0.1:5000"},
		{name: "metadata realm from allowlisted registry", realm: "https://169.254.169.254/token", registryHost: "registry.internal.test:5000", allowed: []string{"registry.internal.test:5000"}, wantErr: true},
		{name: "unrelated host", realm: "https://evil.example/token", registryHost: "ghcr.io", wantErr: true},
		{name: "lookalike registrable domain", realm: "https://token.evil-ghcr.io/token", registryHost: "ghcr.io", wantErr: true},
		{name: "http for https registry", realm: "http://ghcr.io/token", registryHost: "ghcr.io", wantErr: true},
		{name: "http other authority", realm: "http://127.0.0.1:5001/token", registryHost: "127.0.0.1:5000", wantErr: true},
		{name: "metadata ip literal", realm: "https://169.254.169.254/token", registryHost: "ghcr.io", wantErr: true},
		{name: "loopback ip literal", realm: "https://127.0.0.1/token", registryHost: "ghcr.io", wantErr: true},
		{name: "private ip literal", realm: "https://10.1.2.3/token", registryHost: "ghcr.io", wantErr: true},
		{name: "localhost name", realm: "https://localhost/token", registryHost: "ghcr.io", wantErr: true},
		{name: "credentialed realm", realm: "https://user:pass@ghcr.io/token", registryHost: "ghcr.io", wantErr: true},
		{name: "relative realm", realm: "/token", registryHost: "ghcr.io", wantErr: true},
		{name: "non http scheme", realm: "file:///etc/passwd", registryHost: "ghcr.io", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tokenRealmURL(ctx, tc.realm, tc.registryHost, tc.allowed)
			if tc.wantErr && err == nil {
				t.Fatalf("tokenRealmURL(%q, %q) accepted, want rejection", tc.realm, tc.registryHost)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("tokenRealmURL(%q, %q) = %v, want ok", tc.realm, tc.registryHost, err)
			}
		})
	}
}

func TestTokenRealmURLBlocksPrivateResolution(t *testing.T) {
	stubLookup(t, "127.0.0.1")
	ctx := context.Background()
	// A sibling of the registry's own site resolves to loopback and is
	// not the registry's exact host, so following it would walk the
	// control plane into internal addresses.
	if _, err := tokenRealmURL(ctx, "https://token.evil.com/token", "evil.com", nil); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("tokenRealmURL resolved private destination without rejection: %v", err)
	}
}

// TestTokenRealmURLAllowsAllowlistedPrivateSibling proves the operator
// escape hatch end to end: an allowlisted internal registry may keep its
// token realm on a private sibling host, while the same sibling is
// refused without the allowlist entry.
func TestTokenRealmURLAllowsAllowlistedPrivateSibling(t *testing.T) {
	stubLookup(t, "10.1.2.3")
	ctx := context.Background()
	if _, err := tokenRealmURL(ctx, "https://auth.internal.test/token", "registry.internal.test:5000", nil); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("tokenRealmURL accepted unlisted private sibling: %v", err)
	}
	if _, err := tokenRealmURL(ctx, "https://auth.internal.test/token", "registry.internal.test:5000", []string{"registry.internal.test:5000"}); err != nil {
		t.Fatalf("tokenRealmURL rejected allowlisted private sibling: %v", err)
	}
}

func TestHTTPResolverRefusesHostileTokenRealm(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://169.254.169.254/latest/meta-data/",service="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
	_, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo:latest")
	if err == nil || !strings.Contains(err.Error(), "registry auth realm") {
		t.Fatalf("Resolve = %v, want registry auth realm rejection", err)
	}
}

func TestAnonymousTokenRefusesRedirects(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	}))
	defer server.Close()
	resolver := NewHTTPResolver(server.Client(), nil)
	host := strings.TrimPrefix(server.URL, "http://")
	_, err := resolver.anonymousToken(context.Background(), bearerChallengeValues{realm: server.URL + "/token"}, host, "demo/echo")
	if err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("anonymousToken = %v, want redirect refusal", err)
	}
}
