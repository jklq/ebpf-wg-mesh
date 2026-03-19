package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/metadata"
)

func TestValidatorIdentityFromContext(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksPath := filepath.Join(t.TempDir(), "jwks.json")
	if err := writeJWKS(jwksPath, "test-key", &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	validator, err := NewValidator(config.OIDCConfig{
		Issuer:             "https://issuer.example",
		Audience:           "platform",
		JWKSURL:            jwksPath,
		AllowedEmailDomain: "example.com",
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "user-1",
		"email": "user@example.com",
		"iss":   "https://issuer.example",
		"aud":   []string{"platform"},
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+raw))
	identity, err := validator.identityFromContext(ctx)
	if err != nil {
		t.Fatalf("identityFromContext: %v", err)
	}
	if identity.Subject != "user-1" {
		t.Fatalf("unexpected subject %q", identity.Subject)
	}
}

func TestValidatorRefreshesJWKSOnUnknownKID(t *testing.T) {
	t.Parallel()

	key1, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key2, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksPath := filepath.Join(t.TempDir(), "jwks.json")
	if err := writeJWKS(jwksPath, "key-1", &key1.PublicKey); err != nil {
		t.Fatal(err)
	}

	validator, err := NewValidator(config.OIDCConfig{
		Issuer:             "https://issuer.example",
		Audience:           "platform",
		JWKSURL:            jwksPath,
		AllowedEmailDomain: "example.com",
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	if _, err := validator.identityFromContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+mustSignToken(t, key1, "key-1", "user-1", "user@example.com"),
	))); err != nil {
		t.Fatalf("identityFromContext initial key: %v", err)
	}

	if err := writeJWKS(jwksPath, "key-2", &key2.PublicKey); err != nil {
		t.Fatal(err)
	}

	identity, err := validator.identityFromContext(metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+mustSignToken(t, key2, "key-2", "user-2", "user@example.com"),
	)))
	if err != nil {
		t.Fatalf("identityFromContext rotated key: %v", err)
	}
	if identity.Subject != "user-2" {
		t.Fatalf("unexpected subject %q", identity.Subject)
	}
}

func writeJWKS(path, kid string, key *rsa.PublicKey) error {
	jwks := map[string]any{
		"keys": []map[string]string{{
			"kid": kid,
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
		}},
	}
	b, _ := json.Marshal(jwks)
	return os.WriteFile(path, b, 0o644)
}

func mustSignToken(t *testing.T, key *rsa.PrivateKey, kid, subject, email string) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   subject,
		"email": email,
		"iss":   "https://issuer.example",
		"aud":   []string{"platform"},
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = kid
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return raw
}
