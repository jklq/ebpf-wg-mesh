package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPResolverPinnedRefSkipsNetwork(t *testing.T) {
	t.Parallel()

	digest := "sha256:" + strings.Repeat("c3", 32)
	resolver := NewHTTPResolver(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("pinned reference must not touch the network")
		return nil, errors.New("network used")
	})})
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

	resolver := NewHTTPResolver(server.Client())
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

	resolver := NewHTTPResolver(server.Client())
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

	resolver := NewHTTPResolver(server.Client())
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

	resolver := NewHTTPResolver(server.Client())
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
	} {
		if got := manifestURLFor(tc.repository, tc.reference); got != tc.want {
			t.Fatalf("manifestURLFor(%q, %q) = %q, want %q", tc.repository, tc.reference, got, tc.want)
		}
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
