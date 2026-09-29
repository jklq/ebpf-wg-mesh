package signkeys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
	"unicode/utf8"
)

// CACertValidity is the self-signed certificate lifetime for generated CA and
// registry signer keys. It only bounds a forgotten key; rotation is independent.
const CACertValidity = 10 * 365 * 24 * time.Hour

// HMACSecretSize is the random entropy behind a generated HMAC secret, stored
// base64url-encoded so the bytes travel safely through environment files.
const HMACSecretSize = 32

// MinHMACSecretLength is the shortest operator-supplied HMAC secret in bytes.
const MinHMACSecretLength = 32

const MaxHMACSecretLength = 1024

// ecdsaIdentity is one generated signing identity: wrapped PKCS#8 private key
// plus the self-signed certificate stored alongside.
type ecdsaIdentity struct {
	keyDER  []byte
	cert    *x509.Certificate
	certPEM []byte
}

func generateCAIdentity() (*ecdsaIdentity, error) {
	return generateSelfSignedIdentity("ebpf-wg-mesh internal ca", x509.KeyUsageCertSign|x509.KeyUsageCRLSign, true)
}

// generateRegistryIdentity mints a fresh self-signed registry token signer.
func generateRegistryIdentity(issuer string) (*ecdsaIdentity, error) {
	if issuer == "" {
		issuer = "ebpf-wg-mesh"
	}
	return generateSelfSignedIdentity(issuer+" registry token signer", x509.KeyUsageDigitalSignature|x509.KeyUsageCertSign, true)
}

func generateSelfSignedIdentity(commonName string, usage x509.KeyUsage, isCA bool) (*ecdsaIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(CACertValidity),
		KeyUsage:              usage,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("create signing certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal signing key: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse signing certificate: %w", err)
	}
	return &ecdsaIdentity{
		keyDER:  keyDER,
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// generateHMACSecret returns fresh secret bytes as base64url-encoded UTF-8.
func generateHMACSecret() ([]byte, error) {
	raw := make([]byte, HMACSecretSize)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate HMAC secret: %w", err)
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw)), nil
}

// validateHMACSecret rejects secrets that would not survive dashboard transport
// or are too short to sign with.
func validateHMACSecret(secret []byte) error {
	if len(secret) < MinHMACSecretLength || len(secret) > MaxHMACSecretLength {
		return fmt.Errorf("%w: must be %d-%d bytes", ErrHMACSecretInvalid, MinHMACSecretLength, MaxHMACSecretLength)
	}
	if !utf8.Valid(secret) {
		return fmt.Errorf("%w: must be valid UTF-8 for dashboard transport", ErrHMACSecretInvalid)
	}
	trimmed := len(secret)
	for trimmed > 0 && isHMACSpace(secret[trimmed-1]) {
		trimmed--
	}
	leading := 0
	for leading < len(secret) && isHMACSpace(secret[leading]) {
		leading++
	}
	if leading > 0 || trimmed != len(secret) {
		return fmt.Errorf("%w: must not have leading or trailing whitespace", ErrHMACSecretInvalid)
	}
	return nil
}

func isHMACSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

func parseECDSAPrivateKey(der []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("signing key must be ECDSA P-256")
	}
	return key, nil
}

func parseCertificatePEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("decode signing certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing certificate: %w", err)
	}
	return cert, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, fmt.Errorf("generate certificate serial: %w", err)
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

func generateKeyID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate signing key id: %w", err)
	}
	return "sk-" + hex.EncodeToString(raw[:]), nil
}

func generateKID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate signing kid: %w", err)
	}
	return "kid-" + hex.EncodeToString(raw[:]), nil
}
