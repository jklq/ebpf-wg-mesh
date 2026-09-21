// Package signkeystest is an in-memory signkeys.Provider for unit tests.
// Consumer packages (identity, registry, agent helpers) test rotation
// behavior against it without a database; the database-backed Service has
// its own integration suite.
package signkeystest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/signkeys"
)

// Fake holds one active and one optional retiring key per scope.
type Fake struct {
	mu   sync.Mutex
	keys map[string]*fakeScope
}

type fakeScope struct {
	active   signkeys.Material
	retiring *signkeys.Material
}

// New seeds every scope with a fresh active key.
func New(t TestingT) *Fake {
	t.Helper()
	f := &Fake{keys: map[string]*fakeScope{}}
	for _, scope := range signkeys.AllScopes() {
		mat, err := generateMaterial(scope, signkeys.KeyStateActive)
		if err != nil {
			t.Fatalf("seed signing scope %s: %v", scope, err)
		}
		f.keys[scope] = &fakeScope{active: mat}
	}
	return f
}

// TestingT covers *testing.T without importing testing here.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
}

var _ signkeys.Provider = (*Fake)(nil)

// Active returns the scope's signing key.
func (f *Fake) Active(_ context.Context, scope string) (signkeys.Material, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.keys[scope]
	if !ok {
		return signkeys.Material{}, fmt.Errorf("%w: %q", signkeys.ErrUnknownScope, scope)
	}
	return s.active, nil
}

// Verifying returns the active key first, then the retiring key while a
// rotation overlaps.
func (f *Fake) Verifying(_ context.Context, scope string) ([]signkeys.Material, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.keys[scope]
	if !ok {
		return nil, fmt.Errorf("%w: %q", signkeys.ErrUnknownScope, scope)
	}
	out := []signkeys.Material{s.active}
	if s.retiring != nil {
		out = append(out, *s.retiring)
	}
	return out, nil
}

// PublicBundle concatenates the verifying certificates, active first.
func (f *Fake) PublicBundle(ctx context.Context, scope string) ([]byte, error) {
	mats, err := f.Verifying(ctx, scope)
	if err != nil {
		return nil, err
	}
	var bundle []byte
	for _, mat := range mats {
		if mat.Record.KeyType != signkeys.KeyTypeECDSAP256 {
			return nil, fmt.Errorf("scope %q holds no certificates", scope)
		}
		if len(bytes.TrimSpace(mat.Record.PublicPEM)) == 0 {
			return nil, fmt.Errorf("signing key %s has no certificate", mat.Record.ID)
		}
		bundle = append(bundle, bytes.TrimSpace(mat.Record.PublicPEM)...)
		bundle = append(bundle, '\n')
	}
	return bundle, nil
}

// Rotate demotes the active key to retiring and activates a fresh key,
// mirroring Service.RotateStart without overlap bookkeeping.
func (f *Fake) Rotate(t TestingT, scope string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.keys[scope]
	if !ok {
		t.Fatalf("rotate unknown scope %q", scope)
	}
	if s.retiring != nil {
		t.Fatalf("scope %q is already mid-rotation", scope)
	}
	mat, err := generateMaterial(scope, signkeys.KeyStateActive)
	if err != nil {
		t.Fatalf("rotate scope %s: %v", scope, err)
	}
	retiring := s.active
	retiring.Record.State = signkeys.KeyStateRetiring
	now := time.Now().UTC()
	retiring.Record.RetiredAt = &now
	s.retiring = &retiring
	s.active = mat
}

// Finish drops the retiring key, mirroring Service.RotateFinish.
func (f *Fake) Finish(t TestingT, scope string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.keys[scope]
	if !ok {
		t.Fatalf("finish unknown scope %q", scope)
	}
	if s.retiring == nil {
		t.Fatalf("scope %q has no rotation in progress", scope)
	}
	s.retiring = nil
}

// ActiveSecret returns the active HMAC secret for a dashboard-held scope.
func (f *Fake) ActiveSecret(t TestingT, scope string) []byte {
	t.Helper()
	mat, err := f.Active(context.Background(), scope)
	if err != nil {
		t.Fatalf("active secret for scope %s: %v", scope, err)
	}
	if mat.Record.KeyType != signkeys.KeyTypeHMAC256 {
		t.Fatalf("scope %q holds no HMAC secret", scope)
	}
	return append([]byte(nil), mat.Private...)
}

var fakeSerialMu sync.Mutex
var fakeSerial = big.NewInt(1)

func generateMaterial(scope, state string) (signkeys.Material, error) {
	keyType, err := signkeys.KeyTypeForScope(scope)
	if err != nil {
		return signkeys.Material{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return signkeys.Material{}, err
	}
	tag := fmt.Sprintf("%x", idBytes)
	rec := signkeys.Record{
		ID:            "test-" + scope + "-" + tag,
		Scope:         scope,
		KID:           "test-kid-" + tag,
		State:         state,
		KeyType:       keyType,
		WrappingKeyID: "test-envelope-key",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	mat := signkeys.Material{Record: rec}
	switch keyType {
	case signkeys.KeyTypeECDSAP256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return signkeys.Material{}, err
		}
		fakeSerialMu.Lock()
		serial := new(big.Int).Add(fakeSerial, big.NewInt(1))
		fakeSerial = serial
		fakeSerialMu.Unlock()
		now := time.Now().UTC()
		template := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: "test " + scope + " ca"},
			NotBefore:             now.Add(-time.Minute),
			NotAfter:              now.Add(365 * 24 * time.Hour),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		if err != nil {
			return signkeys.Material{}, err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return signkeys.Material{}, err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return signkeys.Material{}, err
		}
		mat.Private = keyDER
		mat.Key = key
		mat.Cert = cert
		mat.Record.PublicPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	case signkeys.KeyTypeHMAC256:
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return signkeys.Material{}, err
		}
		mat.Private = []byte(base64.RawURLEncoding.EncodeToString(raw))
	default:
		return signkeys.Material{}, fmt.Errorf("unknown key type %q", keyType)
	}
	return mat, nil
}
