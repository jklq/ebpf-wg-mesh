package secretkeys

import (
	"bytes"
	"strings"
	"testing"
)

func TestSealOpenValueRoundTrip(t *testing.T) {
	t.Parallel()

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("sealed/v1\x00service\x00NAME\x001")
	nonce, ciphertext, err := SealValue(dek, aad, []byte("super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != NonceSize {
		t.Fatalf("nonce length %d", len(nonce))
	}
	plaintext, err := OpenValue(dek, aad, nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "super-secret" {
		t.Fatalf("round trip = %q", plaintext)
	}
}

func TestOpenValueRejectsTampering(t *testing.T) {
	t.Parallel()

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("sealed/v1\x00service\x00NAME\x001")
	nonce, ciphertext, err := SealValue(dek, aad, []byte("super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(ciphertext)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := OpenValue(dek, aad, nonce, tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	badNonce := bytes.Clone(nonce)
	badNonce[0] ^= 0xff
	if _, err := OpenValue(dek, aad, badNonce, ciphertext); err == nil {
		t.Fatal("tampered nonce was accepted")
	}
	if _, err := OpenValue(dek, []byte("sealed/v1\x00other\x00NAME\x001"), nonce, ciphertext); err == nil {
		t.Fatal("transplanted AAD was accepted")
	}
	other, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenValue(other, aad, nonce, ciphertext); err == nil {
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
