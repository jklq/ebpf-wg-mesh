package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
	"github.com/golang-jwt/jwt/v5"
)

func TestDeleteImageUsesCollectorOnlyCapabilityAndRetries(t *testing.T) {
	ctx := context.Background()
	keys := signkeystest.New(t)
	cfg := testRegistryConfig()
	auth, err := NewAuth(ctx, cfg, keys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	active, err := keys.Active(ctx, signkeys.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusAccepted
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || !strings.HasPrefix(r.URL.Path, "/v2/mesh/p/e/b/s/manifests/sha256:") {
			t.Errorf("delete request: %s %s", r.Method, r.URL.Path)
		}
		claims := &registryTokenClaims{}
		if _, err := jwt.ParseWithClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), claims,
			func(*jwt.Token) (any, error) { return &active.Key.PublicKey, nil }, jwt.WithValidMethods([]string{"ES256"})); err != nil {
			t.Error(err)
		}
		if len(claims.Access) != 1 || claims.Access[0].Name != "mesh/p/e/b/s" || !sameStrings(claims.Access[0].Actions, []string{"delete"}) {
			t.Errorf("delete grants: %+v", claims.Access)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	cfg.Host = strings.TrimPrefix(server.URL, "http://")
	d := NewDeleter(cfg, auth)
	ref := cfg.Host + "/mesh/p/e/b/s@sha256:" + strings.Repeat("a", 64)
	for _, status = range []int{http.StatusAccepted, http.StatusNotFound} {
		if err := d.DeleteImage(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	status = http.StatusServiceUnavailable
	if err := d.DeleteImage(ctx, ref); err == nil {
		t.Fatal("server failure must leave deletion retryable")
	}
	for _, invalid := range []string{strings.Replace(ref, "/mesh/", "/other/", 1), "external.example.test/mesh/p/e/b/s@sha256:" + strings.Repeat("a", 64)} {
		if err := d.DeleteImage(ctx, invalid); err == nil {
			t.Fatal("deletion escaped platform registry namespace")
		}
	}
	if calls != 3 {
		t.Fatalf("delete requests = %d", calls)
	}
	d.SetRecoveryProtection(func(context.Context, string) error { return errors.New("protected copy unavailable") })
	if err := d.DeleteImage(ctx, ref); err == nil || calls != 3 {
		t.Fatal("active deletion bypassed recovery protection", err, calls)
	}
	if _, _, err := auth.MintCredential(ctx, "builder", "mesh/p/e/b/s", []string{"delete"}, nil); err == nil {
		t.Fatal("ordinary capabilities must not grant delete")
	}
}
