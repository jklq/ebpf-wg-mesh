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

// NonceSize is the AES-GCM nonce size used for encrypted values and file wraps.
const NonceSize = 12

// GenerateDEK returns fresh random data-encryption key material.
func GenerateDEK() ([DEKSize]byte, error) {
	var dek [DEKSize]byte
	if _, err := rand.Read(dek[:]); err != nil {
		return dek, fmt.Errorf("generate data-encryption key: %w", err)
	}
	return dek, nil
}

// GenerateKeyID returns a random opaque registry key identifier.
func GenerateKeyID() (string, error) {
	return generatePrefixedID("kek-")
}

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

// EncryptValue encrypts plaintext with dek under AAD binding the ciphertext to
// its location, so a copied row does not decrypt. The random nonce prefixes the
// returned bytes; callers persist them as one opaque value.
func EncryptValue(dek [DEKSize]byte, aad, plaintext []byte) ([]byte, error) {
	return sealWithNonce(dek[:], aad, plaintext)
}

// DecryptValue opens bytes produced by EncryptValue. Tampering reports an
// authentication failure without detail that could aid forgery.
func DecryptValue(dek [DEKSize]byte, aad, data []byte) ([]byte, error) {
	if len(data) < NonceSize {
		return nil, errors.New("encrypted value is truncated")
	}
	aead, err := newCipher(dek[:])
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, data[:NonceSize], data[NonceSize:], aad)
	if err != nil {
		return nil, errors.New("encrypted value authentication failed")
	}
	return plaintext, nil
}

// sealWithNonce encrypts plaintext with a fresh nonce and prefixes it.
func sealWithNonce(key, aad, plaintext []byte) ([]byte, error) {
	aead, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
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
