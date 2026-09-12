package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/golang-jwt/jwt/v5"
)

func TestPolicyMintsExactBuildScopedCapability(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	cfg := testRegistryConfig()
	auth, err := NewAuth(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now }
	policy := NewPolicy(cfg, auth)
	policy.now = auth.now
	pushRef := policy.PushRef("project-1", "environment-1", "build-7", "service-2", "deadbeef")
	if want := "registry.example.test:5000/mesh/project-1/environment-1/build-7/service-2:git-deadbeef"; pushRef != want {
		t.Fatalf("push ref = %q, want %q", pushRef, want)
	}
	username, password, err := policy.CredentialsForBuild(context.Background(), "project-1", "build-7", pushRef)
	if err != nil {
		t.Fatalf("CredentialsForBuild: %v", err)
	}
	claims, err := auth.parseCapability(username, password)
	if err != nil {
		t.Fatalf("parse capability: %v", err)
	}
	if username != "build-build-7" || claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("unexpected identity or expiry: username=%q claims=%+v", username, claims.RegisteredClaims)
	}
	wantAccess := registryAccess{Type: "repository", Name: "mesh/project-1/environment-1/build-7/service-2", Actions: []string{"pull", "push"}}
	if len(claims.Access) != 1 || claims.Access[0].Type != wantAccess.Type || claims.Access[0].Name != wantAccess.Name || !sameStrings(claims.Access[0].Actions, wantAccess.Actions) {
		t.Fatalf("unexpected capability access %+v", claims.Access)
	}
}

func TestAuthIntersectsRequestedScope(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	auth, err := NewAuth(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	username, password, err := auth.MintCredential("agent-1", "mesh/project-1/build-1/service-1", []string{"pull"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requestToken := func(scope string) registryTokenClaims {
		t.Helper()
		query := url.Values{"service": {cfg.TokenService}, "scope": {scope}}
		req := httptest.NewRequest(http.MethodGet, TokenPath+"?"+query.Encode(), nil)
		req.SetBasicAuth(username, password)
		resp := httptest.NewRecorder()
		auth.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("token response = %d: %s", resp.Code, resp.Body.String())
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		claims := registryTokenClaims{}
		parsed, err := jwt.ParseWithClaims(body.Token, &claims, func(*jwt.Token) (any, error) {
			return &auth.key.PublicKey, nil
		}, jwt.WithIssuer(cfg.TokenIssuer), jwt.WithAudience(cfg.TokenService))
		if err != nil || !parsed.Valid {
			t.Fatalf("parse registry token: %v", err)
		}
		return claims
	}

	allowed := requestToken("repository:mesh/project-1/build-1/service-1:pull,push")
	if len(allowed.Access) != 1 || !sameStrings(allowed.Access[0].Actions, []string{"pull"}) {
		t.Fatalf("unexpected allowed access %+v", allowed.Access)
	}
	denied := requestToken("repository:mesh/project-2/build-1/service-1:pull")
	if len(denied.Access) != 0 {
		t.Fatalf("sibling repository was authorized: %+v", denied.Access)
	}
}

func TestAuthRejectsExpiredOrAlteredCredentials(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	cfg := testRegistryConfig()
	auth, err := NewAuth(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now }
	expiresAt := now.Add(time.Minute)
	username, password, err := auth.MintCredential("builder", "mesh/project/build/service", []string{"push"}, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){
		"expired": func() { auth.now = func() time.Time { return now.Add(2 * time.Minute) } },
		"altered": func() { password += "x" },
	} {
		t.Run(name, func(t *testing.T) {
			originalNow, originalPassword := auth.now, password
			defer func() { auth.now, password = originalNow, originalPassword }()
			mutate()
			req := httptest.NewRequest(http.MethodGet, TokenPath+"?service="+url.QueryEscape(cfg.TokenService), nil)
			req.SetBasicAuth(username, password)
			resp := httptest.NewRecorder()
			auth.ServeHTTP(resp, req)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.Code)
			}
		})
	}
}

func TestAuthPersistsSigningIdentity(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	stateDir := t.TempDir()
	first, err := NewAuth(cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	username, password, err := first.MintCredential("agent-1", "mesh/project/environment/build/service", []string{"pull"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAuth(cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.parseCapability(username, password); err != nil {
		t.Fatalf("credential did not survive auth component restart: %v", err)
	}
	if !first.certificate.Equal(second.certificate) {
		t.Fatal("registry signing certificate changed across restart")
	}
}

func TestPolicyRejectsForeignAssignedRepository(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	auth, err := NewAuth(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy := NewPolicy(cfg, auth)
	_, _, err = policy.CredentialsForBuild(context.Background(), "project-1", "build-7", "registry.example.test:5000/mesh/project-2/environment-1/build-7/service:tag")
	if err == nil || !strings.Contains(err.Error(), "does not match project") {
		t.Fatalf("unexpected error %v", err)
	}
}

func testRegistryConfig() config.RegistryConfig {
	return config.RegistryConfig{
		Host:                 "registry.example.test:5000",
		NamespacePrefix:      "mesh",
		AuthListen:           "127.0.0.1:0",
		TokenIssuer:          "registry-test",
		TokenService:         "registry.example.test:5000",
		CredentialTTLSeconds: 300,
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
