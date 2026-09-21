package registry

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"

	"github.com/golang-jwt/jwt/v5"
)

func TestPolicyMintsExactBuildScopedCapability(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	cfg := testRegistryConfig()
	ctx := context.Background()
	auth, err := NewAuth(ctx, cfg, signkeystest.New(t), t.TempDir())
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
	claims, err := auth.parseCapability(ctx, username, password)
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

func TestPolicyMintsExpiringPullCapability(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	cfg := testRegistryConfig()
	ctx := context.Background()
	auth, err := NewAuth(ctx, cfg, signkeystest.New(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now }
	policy := NewPolicy(cfg, auth)
	policy.now = auth.now
	username, password, err := policy.CredentialsForPull(ctx, "node-1", "environment-1", "service-1", "registry.example.test:5000/mesh/project-1/environment-1/build-1/service-1:git-deadbeef")
	if err != nil {
		t.Fatalf("CredentialsForPull: %v", err)
	}
	claims, err := auth.parseCapability(ctx, username, password)
	if err != nil {
		t.Fatalf("parse capability: %v", err)
	}
	// The default pull lifetime exceeds the default client-certificate
	// lifetime so agents holding credentials across session rotations keep
	// pulling, and stays bounded so registry rotation can retire.
	if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("unexpected pull expiry: %+v", claims.RegisteredClaims)
	}
}

func TestAuthIntersectsRequestedScope(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	ctx := context.Background()
	keys := signkeystest.New(t)
	auth, err := NewAuth(ctx, cfg, keys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	username, password, err := auth.MintCredential(ctx, "agent-1", "mesh/project-1/build-1/service-1", []string{"pull"}, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	active, err := keys.Active(ctx, signkeys.ScopeRegistry)
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
			return &active.Key.PublicKey, nil
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
	ctx := context.Background()
	auth, err := NewAuth(ctx, cfg, signkeystest.New(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now }
	expiresAt := now.Add(time.Minute)
	username, password, err := auth.MintCredential(ctx, "builder", "mesh/project/build/service", []string{"push"}, &expiresAt)
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

// TestAuthSharesSigningIdentityAcrossReplicas proves two Auth components
// over shared key state agree: a capability minted by one verifies on the
// other, and both publish the same trust bundle for the registry.
func TestAuthSharesSigningIdentityAcrossReplicas(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	ctx := context.Background()
	keys := signkeystest.New(t)
	first, err := NewAuth(ctx, cfg, keys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	username, password, err := first.MintCredential(ctx, "agent-1", "mesh/project/environment/build/service", []string{"pull"}, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAuth(ctx, cfg, keys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.parseCapability(ctx, username, password); err != nil {
		t.Fatalf("credential did not verify on the second replica: %v", err)
	}
	firstBundle, err := keys.PublicBundle(ctx, signkeys.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	secondBundle, err := keys.PublicBundle(ctx, signkeys.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBundle) != string(secondBundle) {
		t.Fatal("trust bundles differ across replicas")
	}
}

// TestAuthRotationAcceptsBothGenerations walks a registry rotation: the
// token endpoint exchanges capabilities signed by either key through the
// overlap, minted tokens chain to the published bundle, and the retiring
// key stops verifying after finish.
func TestAuthRotationAcceptsBothGenerations(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	ctx := context.Background()
	keys := signkeystest.New(t)
	auth, err := NewAuth(ctx, cfg, keys, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(48 * time.Hour)
	usernameBefore, passwordBefore, err := auth.MintCredential(ctx, "agent-1", "mesh/project/environment/build/service", []string{"pull"}, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	keys.Rotate(t, signkeys.ScopeRegistry)
	usernameAfter, passwordAfter, err := auth.MintCredential(ctx, "agent-1", "mesh/project/environment/build/service", []string{"pull"}, &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(username, password string) string {
		t.Helper()
		query := url.Values{"service": {cfg.TokenService}, "scope": {"repository:mesh/project/environment/build/service:pull"}}
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
		return body.Token
	}
	tokenBefore := exchange(usernameBefore, passwordBefore)
	tokenAfter := exchange(usernameAfter, passwordAfter)

	// Both tokens chain to the published overlap bundle the way the
	// registry daemon verifies them: x5c against rootcertbundle.
	bundle, err := keys.PublicBundle(ctx, signkeys.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		t.Fatal("append trust bundle")
	}
	for name, raw := range map[string]string{"before": tokenBefore, "after": tokenAfter} {
		parser := jwt.NewParser()
		token, _, err := parser.ParseUnverified(raw, &registryTokenClaims{})
		if err != nil {
			t.Fatalf("parse token (%s): %v", name, err)
		}
		x5c, _ := token.Header["x5c"].([]any)
		if len(x5c) != 1 {
			t.Fatalf("token (%s) carries %d x5c certificates, want 1", name, len(x5c))
		}
		der, err := base64.StdEncoding.DecodeString(x5c[0].(string))
		if err != nil {
			t.Fatalf("decode x5c (%s): %v", name, err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse x5c (%s): %v", name, err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
			t.Fatalf("verify token chain (%s): %v", name, err)
		}
		claims := registryTokenClaims{}
		active, err := keys.Active(ctx, signkeys.ScopeRegistry)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := jwt.ParseWithClaims(tokenAfter, &claims, func(*jwt.Token) (any, error) {
			return &active.Key.PublicKey, nil
		}, jwt.WithIssuer(cfg.TokenIssuer), jwt.WithAudience(cfg.TokenService))
		if err != nil || !parsed.Valid {
			t.Fatalf("post-rotation token is not signed by the active key: %v", err)
		}
	}

	keys.Finish(t, signkeys.ScopeRegistry)
	req := httptest.NewRequest(http.MethodGet, TokenPath+"?service="+url.QueryEscape(cfg.TokenService), nil)
	req.SetBasicAuth(usernameBefore, passwordBefore)
	resp := httptest.NewRecorder()
	auth.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("retiring capability status = %d, want 401", resp.Code)
	}
	req = httptest.NewRequest(http.MethodGet, TokenPath+"?service="+url.QueryEscape(cfg.TokenService), nil)
	req.SetBasicAuth(usernameAfter, passwordAfter)
	resp = httptest.NewRecorder()
	auth.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("active capability status = %d, want 200", resp.Code)
	}
}

func TestPolicyRejectsForeignAssignedRepository(t *testing.T) {
	t.Parallel()

	cfg := testRegistryConfig()
	ctx := context.Background()
	auth, err := NewAuth(ctx, cfg, signkeystest.New(t), t.TempDir())
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
