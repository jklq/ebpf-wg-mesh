package controlplane

import (
	"strings"
	"testing"
)

func TestAPITokenSecretShapeAndHash(t *testing.T) {
	t.Parallel()

	secret, prefix, hash, err := newAPITokenSecret()
	if err != nil {
		t.Fatalf("newAPITokenSecret: %v", err)
	}
	if !looksLikeAPITokenSecret(secret) {
		t.Fatalf("minted secret %q fails its own shape check", secret)
	}
	if prefix != secret[:len(prefix)] || len(prefix) != len(apiTokenSecretPrefix)+8 {
		t.Fatalf("prefix %q is not the display prefix of the secret", prefix)
	}
	if strings.Contains(hash, secret) || hash != hashAPITokenSecret(secret) {
		t.Fatalf("hash is not the sha256 of the secret")
	}
	other, _, otherHash, err := newAPITokenSecret()
	if err != nil {
		t.Fatalf("newAPITokenSecret: %v", err)
	}
	if other == secret || otherHash == hash {
		t.Fatalf("two minted secrets collided")
	}
}

func TestLooksLikeAPITokenSecretRejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "wgm_", "bearer-token", "wgm_not!base64", "wgm_" + strings.Repeat("a", 200)} {
		if looksLikeAPITokenSecret(bad) {
			t.Fatalf("accepted malformed secret %q", bad)
		}
	}
}

func TestNormalizeAPITokenName(t *testing.T) {
	t.Parallel()

	if _, err := normalizeAPITokenName("  "); err != errAPITokenNameRequired {
		t.Fatalf("blank name: got %v", err)
	}
	if _, err := normalizeAPITokenName(strings.Repeat("x", maxAPITokenNameLength+1)); err != errAPITokenNameTooLong {
		t.Fatalf("long name: got %v", err)
	}
	name, err := normalizeAPITokenName("  ci deploy  ")
	if err != nil || name != "ci deploy" {
		t.Fatalf("normalize: %q, %v", name, err)
	}
}
