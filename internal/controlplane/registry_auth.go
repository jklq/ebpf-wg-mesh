package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const RegistryTokenPath = "/v1/registry/token"

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
func (c registryTokenClaims) GetIssuedAt() (*jwt.NumericDate, error)       { return c.IssuedAt, nil }
func (c registryTokenClaims) GetNotBefore() (*jwt.NumericDate, error)      { return c.NotBefore, nil }
func (c registryTokenClaims) GetIssuer() (string, error)                   { return c.Issuer, nil }
func (c registryTokenClaims) GetSubject() (string, error)                  { return c.Subject, nil }
func (c registryTokenClaims) GetAudience() (jwt.ClaimStrings, error) {
	return jwt.ClaimStrings{c.Audience}, nil
}

type RegistryAuth struct {
	issuer          string
	service         string
	credentialTTL   time.Duration
	credentialAud   string
	key             *ecdsa.PrivateKey
	certificate     *x509.Certificate
	certificatePath string
	now             func() time.Time
}

func NewRegistryAuth(cfg config.RegistryConfig, stateDir string) (*RegistryAuth, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, nil
	}
	var (
		key      *ecdsa.PrivateKey
		cert     *x509.Certificate
		certPath string
		err      error
	)
	if strings.TrimSpace(cfg.SigningCertFile) != "" || strings.TrimSpace(cfg.SigningKeyFile) != "" {
		key, cert, certPath, err = loadRegistrySigningIdentityFiles(cfg.SigningKeyFile, cfg.SigningCertFile)
	} else {
		key, cert, certPath, err = loadOrCreateRegistrySigningIdentity(stateDir, cfg.TokenIssuer)
	}
	if err != nil {
		return nil, err
	}
	return &RegistryAuth{
		issuer:          cfg.TokenIssuer,
		service:         cfg.TokenService,
		credentialTTL:   time.Duration(cfg.CredentialTTLSeconds) * time.Second,
		credentialAud:   cfg.TokenService + ":credentials",
		key:             key,
		certificate:     cert,
		certificatePath: certPath,
		now:             time.Now,
	}, nil
}

func (a *RegistryAuth) CertificatePath() string {
	if a == nil {
		return ""
	}
	return a.certificatePath
}

func (a *RegistryAuth) MintCredential(subject, repository string, actions []string, expiresAt *time.Time) (string, string, error) {
	if a == nil || a.key == nil {
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
	password, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(a.key)
	if err != nil {
		return "", "", fmt.Errorf("sign registry credential: %w", err)
	}
	return username, password, nil
}

func (a *RegistryAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != RegistryTokenPath {
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

func (a *RegistryAuth) serveToken(w http.ResponseWriter, r *http.Request) {
	username, password, ok := r.BasicAuth()
	if !ok || username == "" || password == "" {
		a.unauthorized(w)
		return
	}
	capability, err := a.parseCapability(username, password)
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
	// Distribution v2 can validate this chain against rootcertbundle without
	// coupling registry configuration to an implementation-specific key ID.
	token.Header["x5c"] = []string{base64.StdEncoding.EncodeToString(a.certificate.Raw)}
	signed, err := token.SignedString(a.key)
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

func (a *RegistryAuth) parseCapability(username, raw string) (*registryCapabilityClaims, error) {
	claims := &registryCapabilityClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodES256 {
			return nil, errors.New("unexpected signing method")
		}
		return &a.key.PublicKey, nil
	}, jwt.WithIssuer(a.issuer), jwt.WithAudience(a.credentialAud), jwt.WithIssuedAt(), jwt.WithLeeway(5*time.Second), jwt.WithTimeFunc(a.now))
	if err != nil || !token.Valid || claims.Subject != username || len(claims.Access) != 1 {
		return nil, errors.New("invalid registry credential")
	}
	grant := claims.Access[0]
	if grant.Type != "repository" || grant.Name == "" || strings.Contains(grant.Name, "*") || len(normalizedRegistryActions(grant.Actions)) != len(grant.Actions) {
		return nil, errors.New("invalid registry credential scope")
	}
	return claims, nil
}

func (a *RegistryAuth) unauthorized(w http.ResponseWriter) {
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

func loadRegistrySigningIdentityFiles(keyPath, certPath string) (*ecdsa.PrivateKey, *x509.Certificate, string, error) {
	keyPath = strings.TrimSpace(keyPath)
	certPath = strings.TrimSpace(certPath)
	if keyPath == "" || certPath == "" {
		return nil, nil, "", errors.New("registry signing certificate and key files are both required")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read registry signing key: %w", err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read registry signing certificate: %w", err)
	}
	key, cert, err := parseRegistrySigningIdentity(keyPEM, certPEM)
	if err != nil {
		return nil, nil, "", err
	}
	return key, cert, certPath, nil
}

func loadOrCreateRegistrySigningIdentity(stateDir, issuer string) (*ecdsa.PrivateKey, *x509.Certificate, string, error) {
	dir := filepath.Join(stateDir, "registry-auth")
	keyPath := filepath.Join(dir, "signing-key.pem")
	certPath := filepath.Join(dir, "signing-cert.pem")
	keyPEM, keyErr := os.ReadFile(keyPath)
	certPEM, certErr := os.ReadFile(certPath)
	if keyErr == nil && certErr == nil {
		if err := os.Chmod(keyPath, 0o600); err != nil {
			return nil, nil, "", fmt.Errorf("protect registry signing key: %w", err)
		}
		key, cert, err := parseRegistrySigningIdentity(keyPEM, certPEM)
		return key, cert, certPath, err
	}
	if (keyErr == nil) != (certErr == nil) || keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) || certErr != nil && !errors.Is(certErr, os.ErrNotExist) {
		return nil, nil, "", fmt.Errorf("load registry signing identity: key=%v cert=%v", keyErr, certErr)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, "", fmt.Errorf("create registry auth directory: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate registry signing key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate registry certificate serial: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: issuer + " registry token signer"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create registry signing certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal registry signing key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, "", fmt.Errorf("write registry signing key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, nil, "", fmt.Errorf("write registry signing certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, "", err
	}
	return key, cert, certPath, nil
}

func parseRegistrySigningIdentity(keyPEM, certPEM []byte) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	keyBlock, _ := pem.Decode(keyPEM)
	certBlock, _ := pem.Decode(certPEM)
	if keyBlock == nil || certBlock == nil {
		return nil, nil, errors.New("invalid registry signing PEM")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, nil, errors.New("registry signing key must be ECDSA P-256")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	publicKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !publicKey.Equal(&key.PublicKey) {
		return nil, nil, errors.New("registry signing certificate does not match key")
	}
	return key, cert, nil
}
