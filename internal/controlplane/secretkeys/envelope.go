package secretkeys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// DEKSize is the AES-256 data-encryption key size.
const DEKSize = 32

// NonceSize is the AES-GCM nonce size used for sealed values and file wraps.
const NonceSize = 12

// MaxSealedValueSize caps a single sealed secret value at 64 KiB.
const MaxSealedValueSize = 64 * 1024

// ErrSealedValueTooLarge is returned when a sealed value exceeds
// MaxSealedValueSize. It carries the limit, never the value.
var ErrSealedValueTooLarge = errors.New("sealed value exceeds size limit")

// GenerateDEK returns fresh random data-encryption key material.
func GenerateDEK() ([DEKSize]byte, error) {
	var dek [DEKSize]byte
	if _, err := rand.Read(dek[:]); err != nil {
		return dek, fmt.Errorf("generate data-encryption key: %w", err)
	}
	return dek, nil
}

// GenerateKeyID returns a random registry key identifier. Identifiers are
// opaque and carry no key material.
func GenerateKeyID() (string, error) {
	return generatePrefixedID("kek-")
}

// GenerateDEKID returns a random data-encryption key identifier.
func GenerateDEKID() (string, error) {
	return generatePrefixedID("dek-")
}

func generatePrefixedID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate key id: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// GenerateNonce returns a fresh random GCM nonce.
func GenerateNonce() ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, nil
}

// SealValue encrypts plaintext with dek under additional authenticated data.
// The AAD binds the ciphertext to its location (service, name, version) so a
// sealed row copied elsewhere does not decrypt. It returns the random nonce
// alongside the ciphertext; callers persist both.
func SealValue(dek [DEKSize]byte, aad, plaintext []byte) (nonce, ciphertext []byte, err error) {
	if len(plaintext) > MaxSealedValueSize {
		return nil, nil, fmt.Errorf("%w: values are capped at %d bytes", ErrSealedValueTooLarge, MaxSealedValueSize)
	}
	sealed, err := sealWithNonce(dek[:], aad, plaintext, nil)
	if err != nil {
		return nil, nil, err
	}
	return sealed[:NonceSize], sealed[NonceSize:], nil
}

// OpenValue decrypts ciphertext sealed by SealValue. A tampered nonce,
// ciphertext, or AAD reports an authentication failure without detail that
// could aid forgery.
func OpenValue(dek [DEKSize]byte, aad, nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) != NonceSize {
		return nil, errors.New("sealed value has invalid nonce")
	}
	aead, err := newCipher(dek[:])
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("sealed value authentication failed")
	}
	return plaintext, nil
}

// sealWithNonce encrypts plaintext and prefixes the nonce when nonceOut is
// nil (generating a fresh nonce) or uses the supplied buffer layout. It is
// shared by sealed values and the keyring wrap format.
func sealWithNonce(key, aad, plaintext, nonce []byte) ([]byte, error) {
	aead, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	if nonce == nil {
		nonce = make([]byte, NonceSize)
		if _, err := rand.Read(nonce); err != nil {
			return nil, fmt.Errorf("generate nonce: %w", err)
		}
	}
	if len(nonce) != NonceSize {
		return nil, errors.New("seal requires a 96-bit nonce")
	}
	out := make([]byte, 0, NonceSize+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, aad), nil
}

func newCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("envelope cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envelope cipher: %w", err)
	}
	return aead, nil
}
