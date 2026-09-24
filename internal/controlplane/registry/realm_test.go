package registry

import (
	"context"
	"crypto/tls"
	"fmt"
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
	stubLookup(t, "93.184.216.34")
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
		{name: "shared CGNAT ip literal", realm: "https://100.64.0.1/token", registryHost: "ghcr.io", wantErr: true},
		{name: "documentation ip literal", realm: "https://203.0.113.5/token", registryHost: "ghcr.io", wantErr: true},
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
// escape hatch: an allowlisted internal registry may keep its token realm
// on a private sibling host (the realm authority is marked trusted so the
// dial guard lets the request through), while the same sibling is refused
// without the allowlist entry.
func TestTokenRealmURLAllowsAllowlistedPrivateSibling(t *testing.T) {
	stubLookup(t, "10.1.2.3")
	ctx := context.Background()
	if _, err := tokenRealmURL(ctx, "https://auth.internal.test/token", "registry.internal.test:5000", nil); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("tokenRealmURL accepted unlisted private sibling: %v", err)
	}
	// The registry's own allowlist entry must not approve sibling
	// services as token authorities: a compromised registry could aim
	// the token fetch at any same-site private host and read a "token"
	// field out of the response.
	if _, err := tokenRealmURL(ctx, "https://auth.internal.test/token", "registry.internal.test:5000", []string{"registry.internal.test:5000"}); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("tokenRealmURL approved a private sibling realm on the registry's entry alone: %v", err)
	}
	realm, err := tokenRealmURL(ctx, "https://auth.internal.test/token", "registry.internal.test:5000", []string{"registry.internal.test:5000", "auth.internal.test"})
	if err != nil {
		t.Fatalf("tokenRealmURL rejected explicitly approved private sibling realm: %v", err)
	}
	if !realm.trustedAuthority {
		t.Fatal("explicitly approved private sibling realm is not marked trusted for dialing")
	}
	stubLookup(t, "93.184.216.34")
	untrusted, err := tokenRealmURL(ctx, "https://auth.ghcr.io/token", "ghcr.io", nil)
	if err != nil {
		t.Fatalf("tokenRealmURL rejected public sibling realm: %v", err)
	}
	if untrusted.trustedAuthority {
		t.Fatal("realm of an unlisted registry must not be trusted for dialing")
	}
}

// TestProhibitedIPRejectsNonPublicSpecialPurposeRanges is the shared-IP
// regression: Go's IsPrivate misses RFC 6598 shared address space, which
// routes inside provider networks like private space. Every
// non-public special-purpose range must be prohibited for unallowlisted
// registries; genuinely public unicast stays reachable.
func TestProhibitedIPRejectsNonPublicSpecialPurposeRanges(t *testing.T) {
	for _, tc := range []struct {
		ip         string
		prohibited bool
	}{
		{ip: "93.184.216.34", prohibited: false},
		{ip: "8.8.8.8", prohibited: false},
		{ip: "2606:2800:220:1:248:1893:25c8:1946", prohibited: false},
		{ip: "10.1.2.3", prohibited: true},
		{ip: "::ffff:10.1.2.3", prohibited: true},
		{ip: "fd00::1", prohibited: true},
		{ip: "127.0.0.1", prohibited: true},
		{ip: "169.254.169.254", prohibited: true},
		{ip: "100.64.0.1", prohibited: true},
		{ip: "100.127.255.255", prohibited: true},
		{ip: "::ffff:100.64.0.1", prohibited: true},
		{ip: "0.1.2.3", prohibited: true},
		{ip: "192.0.0.9", prohibited: true},
		{ip: "192.0.2.1", prohibited: true},
		{ip: "198.51.100.1", prohibited: true},
		{ip: "203.0.113.1", prohibited: true},
		{ip: "198.18.0.1", prohibited: true},
		{ip: "240.0.0.1", prohibited: true},
		{ip: "255.255.255.255", prohibited: true},
		{ip: "100::1", prohibited: true},
		{ip: "2001:2::1", prohibited: true},
		{ip: "2001:db8::1", prohibited: true},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			if got := prohibitedIP(net.ParseIP(tc.ip)); got != tc.prohibited {
				t.Fatalf("prohibitedIP(%s) = %v, want %v", tc.ip, got, tc.prohibited)
			}
		})
	}
}

// TestHTTPResolverReachesAllowlistedPrivateSiblingTokenRealm is the
// request-level regression for the dial guard rejecting what the realm
// check accepts: an internal registry whose Bearer realm lives on a
// private sibling host resolves end to end when the operator approved
// both endpoints, and without the realm's own entry the private sibling
// is never contacted — the registry's entry alone must not open it.
func TestHTTPResolverReachesAllowlistedPrivateSiblingTokenRealm(t *testing.T) {
	stubLookup(t, "10.1.2.3")
	digest := "sha256:" + strings.Repeat("7a", 32)
	tokenHits := 0
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"sibling-token"}`))
	}))
	t.Cleanup(tokenServer.Close)
	tokenPort := portOf(t, tokenServer.URL)
	manifestHits := 0
	var registryServer *httptest.Server
	registryServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestHits++
		if r.Header.Get("Authorization") == "Bearer sibling-token" {
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.internal.test:`+tokenPort+`/token",service="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registryServer.Close)
	registryPort := portOf(t, registryServer.URL)

	// The fake authorities share the internal.test site with the stub
	// resolver's private answer; the transport's dial maps them onto the
	// real test servers so the flow runs with real connections while the
	// dial guard still makes every routing decision. TLS verification is
	// not under test here.
	client := &http.Client{Transport: &http.Transport{
		DialContext: fakeHostDial(map[string]string{
			"registry.internal.test:" + registryPort: stripScheme(t, registryServer.URL),
			"auth.internal.test:" + tokenPort:        stripScheme(t, tokenServer.URL),
		}),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only local servers
	}}
	ref := "registry.internal.test:" + registryPort + "/demo/echo:latest"

	resolver := NewHTTPResolver(client, []string{"registry.internal.test:" + registryPort, "auth.internal.test:" + tokenPort})
	got, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManifestDigest != digest {
		t.Fatalf("Resolve digest = %q, want %q", got.ManifestDigest, digest)
	}
	// One challenged HEAD and one authorized HEAD.
	if manifestHits != 2 || tokenHits != 1 {
		t.Fatalf("manifest hits = %d, token hits = %d, want the sibling realm fetched", manifestHits, tokenHits)
	}

	// The registry's entry alone must not open the private sibling: a
	// compromised registry naming it as the Bearer realm must not have
	// the control plane probe it or echo its response as a token.
	realmUnapproved := NewHTTPResolver(client, []string{"registry.internal.test:" + registryPort})
	if _, err := realmUnapproved.Resolve(context.Background(), ref); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("Resolve without the realm entry = %v, want prohibited private destination", err)
	}
	if manifestHits != 3 || tokenHits != 1 {
		t.Fatalf("manifest hits = %d, token hits = %d, want the unapproved private realm never contacted", manifestHits, tokenHits)
	}

	unlisted := NewHTTPResolver(client, nil)
	if _, err := unlisted.Resolve(context.Background(), ref); err == nil ||
		!strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("Resolve without allowlist = %v, want prohibited private destination", err)
	}
	if manifestHits != 3 || tokenHits != 1 {
		t.Fatalf("manifest hits = %d, token hits = %d, want no traffic without the allowlist entry", manifestHits, tokenHits)
	}
}

func portOf(t *testing.T, serverURL string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(stripScheme(t, serverURL))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func stripScheme(t *testing.T, serverURL string) string {
	t.Helper()
	for _, scheme := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(serverURL, scheme); ok {
			return rest
		}
	}
	t.Fatalf("server URL %q has no scheme", serverURL)
	return ""
}

// fakeHostDial maps fake host:port authorities onto real local test
// servers so tests exercise the resolver's dial decisions with real
// connections while names stay under the test's control.
func fakeHostDial(mapping map[string]string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		target, ok := mapping[addr]
		if !ok {
			return nil, fmt.Errorf("unexpected dial to %s", addr)
		}
		return dialer.DialContext(ctx, network, target)
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
	host := strings.TrimPrefix(server.URL, "http://")
	resolver := NewHTTPResolver(server.Client(), []string{host})
	_, err := resolver.anonymousToken(context.Background(), bearerChallengeValues{realm: server.URL + "/token"}, host, "demo/echo")
	if err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("anonymousToken = %v, want redirect refusal", err)
	}
}

// TestRegistryHostAllowedMatchesPortsExactly pins the allowlist identity:
// an entry matches its own endpoint only, a bare entry means the default
// port, and other ports must be listed explicitly — one declaration must
// never waive the address checks for a different port.
func TestRegistryHostAllowedMatchesPortsExactly(t *testing.T) {
	entries := []string{"registry.internal.test", "registry.internal.test:5000", "[fd00::1]:5000"}
	for _, tc := range []struct {
		endpoint string
		allowed  bool
	}{
		{endpoint: "registry.internal.test", allowed: true},
		{endpoint: "registry.internal.test:443", allowed: true},
		{endpoint: "REGISTRY.INTERNAL.TEST:443", allowed: true},
		{endpoint: "registry.internal.test:5000", allowed: true},
		{endpoint: "registry.internal.test:8443", allowed: false},
		{endpoint: "registry.internal.test:5001", allowed: false},
		{endpoint: "registry.internal.test:0443", allowed: false},
		{endpoint: "other.internal.test:5000", allowed: false},
		{endpoint: "registry.internal.test.evil:5000", allowed: false},
		{endpoint: "[fd00::1]:5000", allowed: true},
		{endpoint: "[fd00::1]:5001", allowed: false},
		{endpoint: "docker.io", allowed: false},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			if got := registryHostAllowed(tc.endpoint, entries); got != tc.allowed {
				t.Fatalf("registryHostAllowed(%q) = %v, want %v", tc.endpoint, got, tc.allowed)
			}
		})
	}
	if !registryHostAllowed("registry-1.docker.io", []string{"docker.io"}) {
		t.Fatal("docker hub aliases must collapse to one endpoint")
	}
}
