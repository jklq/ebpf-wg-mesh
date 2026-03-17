package controlplane

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type identityKey struct{}

type Identity struct {
	Subject string
	Email   string
}

type jwtClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

type Validator struct {
	issuer             string
	audience           string
	allowedEmailDomain string
	jwksSource         string
	mu                 sync.RWMutex
	keys               map[string]*rsa.PublicKey
	lastRefresh        time.Time
}

const jwksRefreshInterval = 5 * time.Minute

func NewValidator(cfg config.OIDCConfig) (*Validator, error) {
	keys, err := loadJWKS(cfg.JWKSURL)
	if err != nil {
		return nil, err
	}
	return &Validator{
		issuer:             cfg.Issuer,
		audience:           cfg.Audience,
		allowedEmailDomain: cfg.AllowedEmailDomain,
		jwksSource:         cfg.JWKSURL,
		keys:               keys,
		lastRefresh:        time.Now(),
	}, nil
}

func (v *Validator) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		identity, err := v.identityFromContext(ctx)
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, identityKey{}, identity), req)
	}
}

func (v *Validator) identityFromContext(ctx context.Context) (Identity, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "missing metadata")
	}
	authHeaders := md.Get("authorization")
	if len(authHeaders) == 0 {
		return Identity{}, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	return v.identityFromAuthorization(authHeaders[0])
}

func (v *Validator) identityFromHTTPRequest(r *http.Request) (Identity, error) {
	return v.identityFromAuthorization(r.Header.Get("Authorization"))
}

func (v *Validator) identityFromAuthorization(raw string) (Identity, error) {
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return Identity{}, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	claims := &jwtClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience))
	parsed, err := parser.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		key, err := v.keyForKID(kid)
		if err != nil {
			return nil, err
		}
		if key == nil {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return key, nil
	})
	if err != nil || !parsed.Valid {
		return Identity{}, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}
	if claims.Subject == "" {
		return Identity{}, status.Error(codes.Unauthenticated, "missing subject")
	}
	if v.allowedEmailDomain != "" {
		if claims.Email == "" || !strings.HasSuffix(strings.ToLower(claims.Email), "@"+strings.ToLower(v.allowedEmailDomain)) {
			return Identity{}, status.Error(codes.PermissionDenied, "email domain not allowed")
		}
	}
	return Identity{Subject: claims.Subject, Email: claims.Email}, nil
}

func (v *Validator) keyForKID(kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("missing kid")
	}

	v.mu.RLock()
	key := v.keys[kid]
	stale := time.Since(v.lastRefresh) >= jwksRefreshInterval
	v.mu.RUnlock()
	if key != nil && !stale {
		return key, nil
	}

	if err := v.refreshKeys(stale || key == nil); err != nil && key == nil {
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.keys[kid], nil
}

func (v *Validator) refreshKeys(force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !force && time.Since(v.lastRefresh) < jwksRefreshInterval {
		return nil
	}

	keys, err := loadJWKS(v.jwksSource)
	if err != nil {
		return err
	}
	v.keys = keys
	v.lastRefresh = time.Now()
	return nil
}

func IdentityFromContext(ctx context.Context) (Identity, error) {
	value := ctx.Value(identityKey{})
	identity, ok := value.(Identity)
	if !ok || identity.Subject == "" {
		return Identity{}, status.Error(codes.Unauthenticated, "identity missing from context")
	}
	return identity, nil
}

func loadJWKS(source string) (map[string]*rsa.PublicKey, error) {
	var data []byte
	var err error
	switch {
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		resp, err := http.Get(source)
		if err != nil {
			return nil, fmt.Errorf("fetch jwks: %w", err)
		}
		defer resp.Body.Close()
		data, err = io.ReadAll(resp.Body)
	case strings.HasPrefix(source, "file://"):
		data, err = os.ReadFile(strings.TrimPrefix(source, "file://"))
	default:
		data, err = os.ReadFile(source)
	}
	if err != nil {
		return nil, fmt.Errorf("load jwks: %w", err)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, key := range doc.Keys {
		if key.Kty != "RSA" {
			continue
		}
		pub, err := rsaPublicKey(key.N, key.E)
		if err != nil {
			return nil, err
		}
		keys[key.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks contained no rsa keys")
	}
	return keys, nil
}

func rsaPublicKey(nValue, eValue string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nValue)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eValue)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
