package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPResolverPinnedRefSkipsNetwork(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("c3", 32)
	resolver := NewHTTPResolver(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("pinned reference must not touch the network")
		return nil, errors.New("network used")
	})}, nil)
	got, err := resolver.Resolve(context.Background(), "example.test/web@"+digest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != "example.test/web@"+digest || got.ManifestDigest != digest || got.Repository != "example.test/web" {
		t.Fatalf("Resolve = %+v, want pinned passthrough", got)
	}
}

func TestHTTPResolverResolvesTag(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("d4", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/demo/echo/manifests/1.27" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") == "" {
			t.Error("manifest request is missing an Accept header")
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
	got, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo:1.27")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	host := strings.TrimPrefix(server.URL, "http://")
	if got.Repository != host+"/demo/echo" || got.ManifestDigest != digest || got.Ref != host+"/demo/echo@"+digest {
		t.Fatalf("Resolve = %+v, want digest %s", got, digest)
	}
}

func TestHTTPResolverFollowsBearerChallenge(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("e5", 32)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/demo/echo/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="test",scope="repository:demo/echo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
		case "/token":
			if got := r.URL.Query().Get("scope"); got != "repository:demo/echo:pull" {
				t.Errorf("token scope = %q, want repository pull scope", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
	got, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManifestDigest != digest {
		t.Fatalf("Resolve digest = %q, want %q", got.ManifestDigest, digest)
	}
}

func TestHTTPResolverNotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
	_, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo:nope")
	if !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("Resolve error = %v, want ErrImageNotFound", err)
	}
}

func TestHTTPResolverRejectsCredentialedRegistry(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
	_, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo:latest")
	if err == nil || !strings.Contains(err.Error(), "publicly pullable") {
		t.Fatalf("Resolve error = %v, want publicly-pullable guidance", err)
	}
}

func TestManifestURLForRoutesDockerHubToDistributionEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ repository, reference, want string }{
		{"docker.io/library/nginx", "1.27", "https://registry-1.docker.io/v2/library/nginx/manifests/1.27"},
		{"index.docker.io/library/nginx", "latest", "https://registry-1.docker.io/v2/library/nginx/manifests/latest"},
		{"example.test/web", "v1", "https://example.test/v2/web/manifests/v1"},
		{"localhost:5000/web", "v1", "http://localhost:5000/v2/web/manifests/v1"},
		{"127.0.0.1:5000/web", "v1", "http://127.0.0.1:5000/v2/web/manifests/v1"},
		{"[::1]:5000/web", "v1", "http://[::1]:5000/v2/web/manifests/v1"},
		{"::1/web", "v1", "http://::1/v2/web/manifests/v1"},
	} {
		if got := manifestURLFor(tc.repository, tc.reference); got != tc.want {
			t.Fatalf("manifestURLFor(%q, %q) = %q, want %q", tc.repository, tc.reference, got, tc.want)
		}
	}
}

func TestHTTPResolverRefusesManifestRedirects(t *testing.T) {
	t.Parallel()

	for name, refuseHead := range map[string]bool{
		"manifest redirect":     false,
		"get fallback redirect": true,
	} {
		t.Run(name, func(t *testing.T) {
			probed := false
			internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				probed = true
			}))
			t.Cleanup(internal.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead && refuseHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				http.Redirect(w, r, internal.URL+"/internal", http.StatusFound)
			}))
			t.Cleanup(server.Close)

			resolver := NewHTTPResolver(server.Client(), []string{strings.TrimPrefix(server.URL, "http://")})
			_, err := resolver.Resolve(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/demo/echo:latest")
			if err == nil || !strings.Contains(err.Error(), "redirect refused") {
				t.Fatalf("Resolve = %v, want redirect refusal", err)
			}
			if probed {
				t.Fatal("manifest redirect reached the internal destination")
			}
		})
	}
}

// TestHTTPResolverRefusesProhibitedRegistryDestination: the registry host is
// user-controlled, so unlisted loopback/private/link-local destinations are refused first.
func TestHTTPResolverRefusesProhibitedRegistryDestination(t *testing.T) {
	probed := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probed = true
	}))
	t.Cleanup(server.Close)

	for name, host := range map[string]string{
		"loopback literal":      strings.TrimPrefix(server.URL, "http://"),
		"metadata literal":      "169.254.169.254",
		"private literal":       "10.1.2.3:5000",
		"shared CGNAT literal":  "100.64.0.1:5000",
		"documentation literal": "203.0.113.5:5000",
		"localhost name":        "localhost:5000",
		"private resolving":     "registry.internal.test",
	} {
		t.Run(name, func(t *testing.T) {
			stubLookup(t, "10.9.8.7")
			resolver := NewHTTPResolver(server.Client(), nil)
			_, err := resolver.Resolve(context.Background(), host+"/demo/echo:latest")
			if err == nil || !strings.Contains(err.Error(), "prohibited private destination") {
				t.Fatalf("Resolve = %v, want prohibited private destination", err)
			}
		})
	}
	if probed {
		t.Fatal("prohibited registry host was contacted")
	}
}

// TestHTTPResolverRefusesRebindingBetweenCheckAndDial: the transport re-validates at
// every dial and pins the approved address, so a DNS answer flipping public-to-private
// cannot make the control plane probe internal services.
func TestHTTPResolverRefusesRebindingBetweenCheckAndDial(t *testing.T) {
	probed := false
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		probed = true
	}))
	t.Cleanup(internal.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	// A rebinding resolver: the request-time check sees a public answer, the dial-time
	// lookup the attacker-controlled private one.
	lookups := 0
	original := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	t.Cleanup(func() { lookupIPAddr = original })

	resolver := NewHTTPResolver(nil, nil)
	_, err = resolver.Resolve(context.Background(), "rebind.example.test:"+port+"/demo/echo:latest")
	if err == nil || !strings.Contains(err.Error(), "prohibited private destination") {
		t.Fatalf("Resolve = %v, want dial-time prohibited private destination", err)
	}
	if lookups < 2 {
		t.Fatalf("lookups = %d, want the dial to re-validate against a fresh resolution", lookups)
	}
	if probed {
		t.Fatal("rebinding registry host reached the internal service")
	}
}

// The escape hatch: an operator-declared internal registry resolves normally.
func TestHTTPResolverAllowsOperatorApprovedPrivateRegistry(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("ab", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	resolver := NewHTTPResolver(server.Client(), []string{host})
	got, err := resolver.Resolve(context.Background(), host+"/demo/echo:latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManifestDigest != digest {
		t.Fatalf("Resolve digest = %q, want %q", got.ManifestDigest, digest)
	}
}

// TestHTTPResolverNeverProxiesRegistryTraffic: no environment proxy can bypass the dial
// guard — traffic connects to the validated address itself.
func TestHTTPResolverNeverProxiesRegistryTraffic(t *testing.T) {
	registryDigest := "sha256:" + strings.Repeat("a1", 32)
	proxiedDigest := "sha256:" + strings.Repeat("b2", 32)
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.Header().Set("Docker-Content-Digest", proxiedDigest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", registryDigest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(registry.Close)
	registryHost := strings.TrimPrefix(registry.URL, "http://")

	// The proxy sits at a public-looking hostname: if the transport proxied, the request
	// would reach it and come back with the proxied digest.
	stubLookup(t, "93.184.216.34")
	_, proxyPort, err := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.example.test:" + proxyPort}),
		DialContext: fakeHostDial(map[string]string{
			registryHost:                 strings.TrimPrefix(registry.URL, "http://"),
			"93.184.216.34:" + proxyPort: strings.TrimPrefix(proxy.URL, "http://"),
		}),
	}}
	resolver := NewHTTPResolver(client, []string{registryHost})
	got, err := resolver.Resolve(context.Background(), registryHost+"/demo/echo:latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ManifestDigest != registryDigest {
		t.Fatalf("Resolve digest = %q, want the registry's %q", got.ManifestDigest, registryDigest)
	}
	if proxyHits != 0 {
		t.Fatalf("registry traffic traversed the environment proxy (%d hits)", proxyHits)
	}
}

func TestStaticResolver(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("f6", 32)
	resolver := StaticResolver{Tags: map[string]string{"example.test/web:1": digest}}
	got, err := resolver.Resolve(context.Background(), "example.test/web:1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Ref != "example.test/web@"+digest {
		t.Fatalf("Resolve ref = %q, want pinned", got.Ref)
	}
	if _, err := resolver.Resolve(context.Background(), "example.test/web:2"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("Resolve unmapped tag error = %v, want ErrImageNotFound", err)
	}

	converging := StaticResolverForTest()
	first, err := converging.Resolve(context.Background(), "nginx:1.27")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	second, err := converging.Resolve(context.Background(), "docker.io/library/nginx:1.27")
	if err != nil {
		t.Fatalf("Resolve normalized: %v", err)
	}
	if first.Ref != second.Ref {
		t.Fatalf("equivalent tags resolved to %q and %q", first.Ref, second.Ref)
	}
	if err := ValidateManifestDigest(first.ManifestDigest); err != nil {
		t.Fatalf("fallback digest: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDialGuardRejectsAllowlistedHostnameOnOtherPorts: one endpoint's entry never
// waives dial-time checks for another port.
func TestDialGuardRejectsAllowlistedHostnameOnOtherPorts(t *testing.T) {
	ctx := context.Background()
	errSentinel := errors.New("sentinel: passthrough dial")
	var dialed []string
	next := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errSentinel
	}

	stubLookup(t, "10.9.8.7") // private at dial time
	if _, err := dialApprovedAddress(ctx, "tcp", "registry.internal.test:8443", []string{"registry.internal.test"}, next); err == nil || len(dialed) != 0 {
		t.Fatalf("bare entry waived the checks on an unlisted port: err=%v dialed=%v", err, dialed)
	}
	if _, err := dialApprovedAddress(ctx, "tcp", "registry.internal.test:8443", []string{"registry.internal.test:5000"}, next); err == nil || len(dialed) != 0 {
		t.Fatalf("an entry for one port waived the checks on another: err=%v dialed=%v", err, dialed)
	}

	if _, err := dialApprovedAddress(ctx, "tcp", "registry.internal.test:443", []string{"registry.internal.test"}, next); err != errSentinel {
		t.Fatalf("default-port dial for a bare entry = %v, want configured passthrough", err)
	}
	if _, err := dialApprovedAddress(ctx, "tcp", "registry.internal.test:8443", []string{"registry.internal.test:8443"}, next); err != errSentinel {
		t.Fatalf("explicit-port dial = %v, want configured passthrough", err)
	}
	if len(dialed) != 2 {
		t.Fatalf("passthrough dials = %v, want only the listed endpoints", dialed)
	}
}

func TestHTTPResolverTriesLaterRegistryAddressesAfterFailedDial(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("7b", 32)
	served := 0
	registryServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(registryServer.Close)
	registryPort := portOf(t, registryServer.URL)

	// Two permitted addresses, first dead: a reachable registry must still resolve.
	original := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.35")}, {IP: net.ParseIP("93.184.216.34")}}, nil
	}
	t.Cleanup(func() { lookupIPAddr = original })

	client := &http.Client{Transport: &http.Transport{
		DialContext: fakeHostDial(map[string]string{
			"93.184.216.34:" + registryPort: stripScheme(t, registryServer.URL),
		}),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only local servers
	}}
	resolver := NewHTTPResolver(client, nil)
	got, err := resolver.Resolve(context.Background(), "example.test:"+registryPort+"/app:latest")
	if err != nil {
		t.Fatalf("Resolve with a dead first address: %v", err)
	}
	if got.ManifestDigest != digest {
		t.Fatalf("Resolve digest = %q, want %q", got.ManifestDigest, digest)
	}
	if served != 1 {
		t.Fatalf("manifest requests = %d, want exactly one on the reachable address", served)
	}
}
