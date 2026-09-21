package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/signkeys"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const TokenPath = "/v1/registry/token"

type registryAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type registryCapabilityClaims struct {
	Access []registryAccess `json:"access"`
	jwt.RegisteredClaims
}

type registryTokenClaims struct {
	Issuer    string           `json:"iss"`
	Subject   string           `json:"sub"`
	Audience  string           `json:"aud"`
	ExpiresAt *jwt.NumericDate `json:"exp"`
	NotBefore *jwt.NumericDate `json:"nbf"`
	IssuedAt  *jwt.NumericDate `json:"iat"`
	ID        string           `json:"jti"`
	Access    []registryAccess `json:"access"`
}

func (c registryTokenClaims) GetExpirationTime() (*jwt.NumericDate, error) { return c.ExpiresAt, nil }

func (c registryTokenClaims) GetIssuedAt() (*jwt.NumericDate, error) { return c.IssuedAt, nil }

func (c registryTokenClaims) GetNotBefore() (*jwt.NumericDate, error) { return c.NotBefore, nil }

func (c registryTokenClaims) GetIssuer() (string, error) { return c.Issuer, nil }

func (c registryTokenClaims) GetSubject() (string, error) { return c.Subject, nil }

func (c registryTokenClaims) GetAudience() (jwt.ClaimStrings, error) {
	return jwt.ClaimStrings{c.Audience}, nil
}

// Auth mints registry capabilities and exchanges them for short-lived
// registry tokens. The signing key is shared signing-key state: every
// replica signs with the active key and verifies capabilities against the
// active plus retiring keys, so rotation never breaks pulls or pushes.
type Auth struct {
	issuer        string
	service       string
	credentialTTL time.Duration
	credentialAud string
	keys          signkeys.Provider
	bundlePath    string
	now           func() time.Time
}

func NewAuth(ctx context.Context, cfg config.RegistryConfig, keys signkeys.Provider, stateDir string) (*Auth, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, nil
	}
	if keys == nil {
		return nil, errors.New("registry auth requires a signing-key provider")
	}
	bundlePath := filepath.Join(stateDir, "registry-auth", "signing-bundle.pem")
	if err := writeTrustBundle(ctx, keys, bundlePath); err != nil {
		return nil, err
	}
	return &Auth{
		issuer:        cfg.TokenIssuer,
		service:       cfg.TokenService,
		credentialTTL: time.Duration(cfg.CredentialTTLSeconds) * time.Second,
		credentialAud: cfg.TokenService + ":credentials",
		keys:          keys,
		bundlePath:    bundlePath,
		now:           time.Now,
	}, nil
}

// writeTrustBundle publishes the registry trust bundle (active plus
// retiring certificates) for the registry's rootcertbundle mount. The
// bundle is public material; the state directory no longer holds the
// signing key. Rotation refreshes it via `signing-keys export`; see the
// runbook in docs/signing-keys.md.
func writeTrustBundle(ctx context.Context, keys signkeys.Provider, bundlePath string) error {
	bundle, err := keys.PublicBundle(ctx, signkeys.ScopeRegistry)
	if err != nil {
		return fmt.Errorf("load registry trust bundle: %w", err)
	}
	dir := filepath.Dir(bundlePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create registry auth directory: %w", err)
	}
	// Pre-2.3b file-based signer. The key is shared state now; a stale
	// file must never be mistaken for authority.
	for _, stale := range []string{"signing-key.pem", "signing-cert.pem"} {
		_ = os.Remove(filepath.Join(dir, stale))
	}
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		return fmt.Errorf("write registry trust bundle: %w", err)
	}
	return nil
}

// RefreshTrustBundle republishes the trust bundle after a rotation.
func (a *Auth) RefreshTrustBundle(ctx context.Context) error {
	if a == nil {
		return nil
	}
	return writeTrustBundle(ctx, a.keys, a.bundlePath)
}

// BundlePath is the registry trust bundle file: the rootcertbundle the
// registry daemon verifies token signatures against.
func (a *Auth) BundlePath() string {
	if a == nil {
		return ""
	}
	return a.bundlePath
}

func (a *Auth) MintCredential(ctx context.Context, subject, repository string, actions []string, expiresAt *time.Time) (string, string, error) {
	if a == nil || a.keys == nil {
		return "", "", errors.New("registry auth is not configured")
	}
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" || strings.Contains(repository, "*") {
		return "", "", errors.New("exact registry repository is required")
	}
	normalizedActions := normalizedRegistryActions(actions)
	if len(normalizedActions) == 0 || len(normalizedActions) != len(actions) {
		return "", "", errors.New("registry actions must be unique pull and/or push values")
	}
	actions = normalizedActions
	active, err := a.keys.Active(ctx, signkeys.ScopeRegistry)
	if err != nil {
		return "", "", fmt.Errorf("load active registry signing key: %w", err)
	}
	if active.Key == nil {
		return "", "", errors.New("active registry signing key has no material")
	}
	username := sanitizeRefSegment(subject)
	now := a.now().UTC()
	claims := registryCapabilityClaims{
		Access: []registryAccess{{Type: "repository", Name: repository, Actions: actions}},
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    a.issuer,
			Subject:   username,
			Audience:  jwt.ClaimStrings{a.credentialAud},
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
	}
	if expiresAt != nil {
		claims.ExpiresAt = jwt.NewNumericDate(expiresAt.UTC())
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = active.Record.KID
	password, err := token.SignedString(active.Key)
	if err != nil {
		return "", "", fmt.Errorf("sign registry credential: %w", err)
	}
	return username, password, nil
}

func (a *Auth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != TokenPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.serveToken(w, r)
}

func (a *Auth) serveToken(w http.ResponseWriter, r *http.Request) {
	username, password, ok := r.BasicAuth()
	if !ok || username == "" || password == "" {
		a.unauthorized(w)
		return
	}
	capability, err := a.parseCapability(r.Context(), username, password)
	if err != nil {
		a.unauthorized(w)
		return
	}
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	if service == "" {
		service = a.service
	}
	if service != a.service {
		http.Error(w, "invalid registry service", http.StatusBadRequest)
		return
	}
	access := intersectRegistryScopes(r.URL.Query()["scope"], capability.Access)
	active, err := a.keys.Active(r.Context(), signkeys.ScopeRegistry)
	if err != nil || active.Key == nil || active.Cert == nil {
		http.Error(w, "mint registry token", http.StatusInternalServerError)
		return
	}
	now := a.now().UTC()
	claims := registryTokenClaims{
		Issuer:    a.issuer,
		Subject:   username,
		Audience:  a.service,
		ExpiresAt: jwt.NewNumericDate(now.Add(a.credentialTTL)),
		NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
		IssuedAt:  jwt.NewNumericDate(now),
		ID:        uuid.NewString(),
		Access:    access,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = active.Record.KID
	token.Header["x5c"] = []string{base64.StdEncoding.EncodeToString(active.Cert.Raw)}
	signed, err := token.SignedString(active.Key)
	if err != nil {
		http.Error(w, "mint registry token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":        signed,
		"access_token": signed,
		"expires_in":   int64(a.credentialTTL / time.Second),
		"issued_at":    now.Format(time.RFC3339),
	})
}

// parseCapability verifies a capability against the active key first, then
// the retiring key while a rotation overlaps.
func (a *Auth) parseCapability(ctx context.Context, username, raw string) (*registryCapabilityClaims, error) {
	mats, err := a.keys.Verifying(ctx, signkeys.ScopeRegistry)
	if err != nil {
		return nil, err
	}
	for _, mat := range mats {
		if mat.Key == nil {
			continue
		}
		public := mat.Key.PublicKey
		claims := &registryCapabilityClaims{}
		token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodES256 {
				return nil, errors.New("unexpected signing method")
			}
			return &public, nil
		}, jwt.WithIssuer(a.issuer), jwt.WithAudience(a.credentialAud), jwt.WithIssuedAt(), jwt.WithLeeway(5*time.Second), jwt.WithTimeFunc(a.now))
		if err != nil || !token.Valid || claims.Subject != username || len(claims.Access) != 1 {
			continue
		}
		grant := claims.Access[0]
		if grant.Type != "repository" || grant.Name == "" || strings.Contains(grant.Name, "*") || len(normalizedRegistryActions(grant.Actions)) != len(grant.Actions) {
			return nil, errors.New("invalid registry credential scope")
		}
		return claims, nil
	}
	return nil, errors.New("invalid registry credential")
}

func (a *Auth) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="registry-token"`)
	http.Error(w, "invalid registry credential", http.StatusUnauthorized)
}

func intersectRegistryScopes(rawScopes []string, grants []registryAccess) []registryAccess {
	if len(rawScopes) == 0 || len(grants) == 0 {
		return nil
	}
	granted := grants[0]
	allowed := make(map[string]bool, len(granted.Actions))
	for _, action := range granted.Actions {
		allowed[action] = true
	}
	var out []registryAccess
	for _, raw := range rawScopes {
		for _, scope := range strings.Fields(raw) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) != 3 || parts[0] != granted.Type || parts[1] != granted.Name {
				continue
			}
			var actions []string
			for _, action := range strings.Split(parts[2], ",") {
				if allowed[action] && !slices.Contains(actions, action) {
					actions = append(actions, action)
				}
			}
			if len(actions) > 0 {
				slices.Sort(actions)
				out = append(out, registryAccess{Type: parts[0], Name: parts[1], Actions: actions})
			}
		}
	}
	return out
}

func normalizedRegistryActions(actions []string) []string {
	var out []string
	for _, action := range actions {
		if (action == "pull" || action == "push") && !slices.Contains(out, action) {
			out = append(out, action)
		}
	}
	slices.Sort(out)
	return out
}
