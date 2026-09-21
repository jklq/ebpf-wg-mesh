package secretkeys

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// fakeKMS emulates the narrow KMS surface the provider needs: per-key
// AES-GCM material with not-found, disabled, and invalid-ciphertext faults.
type fakeKMS struct {
	mu       sync.Mutex
	keys     map[string][32]byte
	disabled map[string]bool
}

func newFakeKMS(keyIDs ...string) *fakeKMS {
	fake := &fakeKMS{keys: map[string][32]byte{}, disabled: map[string]bool{}}
	for _, id := range keyIDs {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			panic(err)
		}
		fake.keys[id] = key
	}
	return fake
}

func (f *fakeKMS) Encrypt(ctx context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := aws.ToString(in.KeyId)
	key, ok := f.keys[ref]
	if !ok {
		return nil, &types.NotFoundException{Message: aws.String("key not found")}
	}
	if f.disabled[ref] {
		return nil, &types.DisabledException{Message: aws.String("key disabled")}
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	blob := aead.Seal(append([]byte("fake-kms/v1\x00"), nonce...), nonce, in.Plaintext, []byte(ref))
	return &kms.EncryptOutput{CiphertextBlob: blob, KeyId: aws.String(ref)}, nil
}

func (f *fakeKMS) Decrypt(ctx context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := aws.ToString(in.KeyId)
	if ref == "" {
		return nil, &types.NotFoundException{Message: aws.String("key required")}
	}
	key, ok := f.keys[ref]
	if !ok {
		return nil, &types.NotFoundException{Message: aws.String("key not found")}
	}
	if f.disabled[ref] {
		return nil, &types.DisabledException{Message: aws.String("key disabled")}
	}
	blob := in.CiphertextBlob
	if len(blob) < len("fake-kms/v1\x00")+12 {
		return nil, &types.InvalidCiphertextException{Message: aws.String("truncated")}
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	payload := blob[len("fake-kms/v1\x00"):]
	plaintext, err := aead.Open(nil, payload[:12], payload[12:], []byte(ref))
	if err != nil {
		return nil, &types.InvalidCiphertextException{Message: aws.String("authentication failed")}
	}
	return &kms.DecryptOutput{Plaintext: plaintext, KeyId: aws.String(ref)}, nil
}

func testKMSProvider(fake *fakeKMS) *KMSProvider {
	return newKMSProviderForTest(fake, KMSConfig{Region: "us-east-1", KeyID: "default-key"})
}

func TestKMSProviderRoundTrip(t *testing.T) {
	t.Parallel()

	fake := newFakeKMS("default-key", "new-key")
	provider := testKMSProvider(fake)
	ctx := context.Background()
	ref, err := provider.ProvisionKey(ctx, "new-key")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "new-key" {
		t.Fatalf("provision ref = %q", ref)
	}
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := provider.Wrap(ctx, ref, dek[:])
	if err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Unwrap(ctx, ref, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, dek[:]) {
		t.Fatal("kms wrap round trip mismatch")
	}
}

func TestKMSProviderProvisionDefaultsAndVerifies(t *testing.T) {
	t.Parallel()

	provider := testKMSProvider(newFakeKMS("default-key"))
	ctx := context.Background()
	ref, err := provider.ProvisionKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "default-key" {
		t.Fatalf("default provision ref = %q", ref)
	}
	if _, err := provider.ProvisionKey(ctx, "missing-key"); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("provision missing key = %v", err)
	}
}

func TestKMSProviderClassifiesFaults(t *testing.T) {
	t.Parallel()

	fake := newFakeKMS("good-key", "off-key")
	fake.disabled["off-key"] = true
	provider := testKMSProvider(fake)
	ctx := context.Background()
	if _, err := provider.Wrap(ctx, "missing-key", []byte("x")); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("wrap missing key = %v", err)
	}
	if _, err := provider.Wrap(ctx, "off-key", []byte("x")); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("wrap disabled key = %v", err)
	}
	wrapped, err := provider.Wrap(ctx, "good-key", []byte("key-material"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(wrapped)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := provider.Unwrap(ctx, "good-key", tampered); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("unwrap tampered = %v", err)
	}
	if _, err := provider.Unwrap(ctx, "good-key", []byte("short")); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("unwrap truncated = %v", err)
	}
}

func TestKMSProviderRejectsBadInput(t *testing.T) {
	t.Parallel()

	provider := testKMSProvider(newFakeKMS("good-key"))
	ctx := context.Background()
	if _, err := provider.Wrap(ctx, "", []byte("x")); err == nil {
		t.Fatal("empty ref was accepted")
	}
	if _, err := provider.Wrap(ctx, "good-key", nil); err == nil {
		t.Fatal("empty plaintext was accepted")
	}
	if _, err := provider.Unwrap(ctx, "good-key", nil); err == nil {
		t.Fatal("empty wrapped was accepted")
	}
}

func TestKMSProviderErrorsNameKeyNotMaterial(t *testing.T) {
	t.Parallel()

	provider := testKMSProvider(newFakeKMS("good-key"))
	ctx := context.Background()
	secret := []byte("material-that-must-not-leak")
	_, err := provider.Wrap(ctx, "missing-key", secret)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("error leaked key material: %v", err)
	}
}
