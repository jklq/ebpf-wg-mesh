package secretkeys

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncryptDecryptValueRoundTrip(t *testing.T) {
	t.Parallel()

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("service-env/v1\x00service\x001")
	data, err := EncryptValue(dek, aad, []byte("super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := DecryptValue(dek, aad, data)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "super-secret" {
		t.Fatalf("round trip = %q", plaintext)
	}
}

func TestDecryptValueRejectsTampering(t *testing.T) {
	t.Parallel()

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("service-env/v1\x00service\x001")
	data, err := EncryptValue(dek, aad, []byte("super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(data)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := DecryptValue(dek, aad, tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	badNonce := bytes.Clone(data)
	badNonce[0] ^= 0xff
	if _, err := DecryptValue(dek, aad, badNonce); err == nil {
		t.Fatal("tampered nonce was accepted")
	}
	if _, err := DecryptValue(dek, aad, data[:NonceSize-1]); err == nil {
		t.Fatal("truncated value was accepted")
	}
	if _, err := DecryptValue(dek, []byte("service-env/v1\x00other\x001"), data); err == nil {
		t.Fatal("transplanted AAD was accepted")
	}
	other, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptValue(other, aad, data); err == nil {
		t.Fatal("wrong DEK was accepted")
	}
}

func TestGenerateKeyIDUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := GenerateKeyID()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "kek-") {
			t.Fatalf("key id %q missing prefix", id)
		}
		if seen[id] {
			t.Fatalf("duplicate key id %q", id)
		}
		seen[id] = true
	}
}
