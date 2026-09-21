package secretkeys

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testKeyringPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "keys.json")
}

func TestKeyringProvisionWrapRoundTrip(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	version, err := keyring.GenerateKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := keyring.ProvisionKey(ctx, version)
	if err != nil {
		t.Fatal(err)
	}
	if ref != version {
		t.Fatalf("provision ref = %q, want %q", ref, version)
	}
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := keyring.Wrap(ctx, ref, "dek-wrap/v1/dek-test", dek[:])
	if err != nil {
		t.Fatal(err)
	}
	opened, err := keyring.Unwrap(ctx, ref, "dek-wrap/v1/dek-test", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, dek[:]) {
		t.Fatal("keyring wrap round trip mismatch")
	}
}

func TestKeyringProvisionRequiresExistingVersion(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := keyring.ProvisionKey(ctx, ""); err == nil {
		t.Fatal("empty hint was accepted")
	}
	// Nothing is generated as a side effect: the version must exist.
	if _, err := keyring.ProvisionKey(ctx, "v-missing"); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("missing version provision = %v", err)
	}
	if _, err := os.Stat(keyring.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provision created %s", keyring.Path())
	}
	if _, err := keyring.GenerateKey(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.GenerateKey(ctx, "v1"); err == nil {
		t.Fatal("duplicate version was accepted")
	}
	for _, id := range []string{"has space", "has/slash", "../escape", strings.Repeat("V", 129)} {
		if _, err := keyring.GenerateKey(ctx, id); err == nil {
			t.Fatalf("invalid version %q was accepted", id)
		}
	}
}

func TestKeyringWrapRequiresPurpose(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	version, err := keyring.GenerateKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Wrap(ctx, version, "", []byte("x")); err == nil {
		t.Fatal("empty purpose was accepted")
	}
	wrapped, err := keyring.Wrap(ctx, version, "dek-wrap/v1/dek-a", []byte("key-material"))
	if err != nil {
		t.Fatal(err)
	}
	// Wrapped bytes cannot be transplanted across purposes.
	if _, err := keyring.Unwrap(ctx, version, "dek-wrap/v1/dek-b", wrapped); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("transplanted purpose = %v", err)
	}
	// Or across key references.
	other, err := keyring.GenerateKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Unwrap(ctx, other, "dek-wrap/v1/dek-a", wrapped); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("transplanted ref = %v", err)
	}
}

func TestKeyringRejectsTamperedWrap(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	version, err := keyring.GenerateKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := keyring.Wrap(ctx, version, "test/v1", []byte("key-material"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(wrapped)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := keyring.Unwrap(ctx, version, "test/v1", tampered); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("tampered wrap = %v", err)
	}
	if _, err := keyring.Unwrap(ctx, version, "test/v1", []byte{0x7f}); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("unknown version = %v", err)
	}
	if _, err := keyring.Unwrap(ctx, version, "test/v1", []byte{keyringWrapVersion, 1, 2}); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("truncated wrap = %v", err)
	}
}

func TestKeyringFilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyring, err := NewKeyring(filepath.Join(dir, "keys.json"), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := keyring.GenerateKey(ctx, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyring.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("keyring permissions %o", info.Mode().Perm())
	}
	// A 0400 file (operator-hardened) still loads.
	if err := os.Chmod(keyring.Path(), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.LocalKeyVersions(); err != nil {
		t.Fatalf("0400 keyring rejected: %v", err)
	}
	if err := os.Chmod(keyring.Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	// Group/other-readable files fail closed.
	for _, mode := range []os.FileMode{0o640, 0o644, 0o660, 0o666} {
		if err := os.Chmod(keyring.Path(), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := keyring.LocalKeyVersions(); err == nil {
			t.Fatalf("mode %o was accepted", mode)
		}
	}
}

func TestKeyringRejectsInvalidEntries(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not json":      `{oops`,
		"wrong version": `{"version":2,"keys":{}}`,
		"bad id":        `{"version":1,"keys":{"has space":{"algorithm":"AES-256-GCM","created_at":"2026-01-01T00:00:00Z","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}}`,
		"bad algorithm": `{"version":1,"keys":{"v1":{"algorithm":"AES-128-GCM","created_at":"2026-01-01T00:00:00Z","key":"AAAAAAAAAAAAAAAAAAAAAA=="}}}`,
		"short key":     `{"version":1,"keys":{"v1":{"algorithm":"AES-256-GCM","created_at":"2026-01-01T00:00:00Z","key":"AAAAAAAAAAAAAAAAAAAAAA=="}}}`,
		"long key":      `{"version":1,"keys":{"v1":{"algorithm":"AES-256-GCM","created_at":"2026-01-01T00:00:00Z","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}}`,
		"not base64":    `{"version":1,"keys":{"v1":{"algorithm":"AES-256-GCM","created_at":"2026-01-01T00:00:00Z","key":"!!!not-base64!!!"}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := testKeyringPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			keyring, err := NewKeyring(path, KeyringOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := keyring.LocalKeyVersions(); err == nil {
				t.Fatal("invalid keyring was accepted")
			}
		})
	}
}

func TestKeyringBootstrap(t *testing.T) {
	t.Parallel()

	t.Run("generation disabled fails closed", func(t *testing.T) {
		t.Parallel()
		keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := keyring.EnsureBootstrapKey(context.Background()); !errors.Is(err, ErrActiveKeyRequired) {
			t.Fatalf("bootstrap without generation = %v", err)
		}
		if _, err := os.Stat(keyring.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bootstrap created %s", keyring.Path())
		}
	})

	t.Run("generation creates and reuses one key", func(t *testing.T) {
		t.Parallel()
		keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{AllowGenerate: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		first, err := keyring.EnsureBootstrapKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		second, err := keyring.EnsureBootstrapKey(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatalf("bootstrap refs %q and %q differ", first, second)
		}
		if _, err := keyring.ProvisionKey(ctx, first); err != nil {
			t.Fatalf("bootstrap version does not verify: %v", err)
		}
	})

	t.Run("several keys require explicit activation", func(t *testing.T) {
		t.Parallel()
		keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{AllowGenerate: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if _, err := keyring.GenerateKey(ctx, "v1"); err != nil {
			t.Fatal(err)
		}
		if _, err := keyring.GenerateKey(ctx, "v2"); err != nil {
			t.Fatal(err)
		}
		if _, err := keyring.EnsureBootstrapKey(ctx); !errors.Is(err, ErrActiveKeyRequired) {
			t.Fatalf("ambiguous bootstrap = %v", err)
		}
	})
}

func TestKeyringHasKeyMaterial(t *testing.T) {
	t.Parallel()

	keyring, err := NewKeyring(testKeyringPath(t), KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A missing file is an error, not an absence.
	if _, err := keyring.HasKeyMaterial(ctx, "v1"); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("missing file = %v", err)
	}
	if _, err := keyring.GenerateKey(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	present, err := keyring.HasKeyMaterial(ctx, "v1")
	if err != nil || !present {
		t.Fatalf("present = %v, %v", present, err)
	}
	present, err = keyring.HasKeyMaterial(ctx, "v2")
	if err != nil || present {
		t.Fatalf("absent = %v, %v", present, err)
	}
}

func TestKeyringReadThroughSeesProvisionedVersions(t *testing.T) {
	t.Parallel()

	path := testKeyringPath(t)
	first, err := NewKeyring(path, KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewKeyring(path, KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A handle opened before the version exists observes it once another
	// handle provisions it: no restart is needed to pick up a version the
	// operator copies to a running replica's file.
	if _, err := first.LocalKeyVersions(); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("empty keyring = %v", err)
	}
	version, err := second.GenerateKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ProvisionKey(ctx, version); err != nil {
		t.Fatalf("read-through provision = %v", err)
	}
}

func TestKeyringConcurrentProvision(t *testing.T) {
	t.Parallel()

	path := testKeyringPath(t)
	first, err := NewKeyring(path, KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewKeyring(path, KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	versions := make([]string, 8)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keyring := first
			if i%2 == 1 {
				keyring = second
			}
			version, err := keyring.GenerateKey(ctx, "")
			versions[i], errs[i] = version, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, version := range versions {
		if errs[i] != nil {
			t.Fatalf("provision %d: %v", i, errs[i])
		}
		if seen[version] {
			t.Fatalf("duplicate version %q", version)
		}
		seen[version] = true
		if _, err := first.ProvisionKey(ctx, version); err != nil {
			t.Fatalf("verify %q: %v", version, err)
		}
	}
}

func TestSealValueOversizeSentinel(t *testing.T) {
	t.Parallel()

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := SealValue(dek, []byte("aad"), make([]byte, MaxSealedValueSize+1)); !errors.Is(err, ErrSealedValueTooLarge) {
		t.Fatalf("oversize seal = %v", err)
	}
	// At exactly the cap the value seals.
	if _, _, err := SealValue(dek, []byte("aad"), make([]byte, MaxSealedValueSize)); err != nil {
		t.Fatalf("capped seal: %v", err)
	}
}
