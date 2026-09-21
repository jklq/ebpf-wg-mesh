package secretkeys

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileProviderRoundTrip(t *testing.T) {
	t.Parallel()

	provider, err := NewFileProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := provider.ProvisionKey(ctx, "")
	if err != nil {
		t.Fatal(err)
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
		t.Fatal("file wrap round trip mismatch")
	}
}

func TestFileProviderKeyFilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	provider, err := NewFileProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file permissions %o", info.Mode().Perm())
	}
	if _, err := provider.ProvisionKey(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten key file permissions %o", info.Mode().Perm())
	}
}

func TestFileProviderUnknownRef(t *testing.T) {
	t.Parallel()

	provider, err := NewFileProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := provider.Wrap(ctx, "file-missing", []byte("x")); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("wrap unknown ref = %v", err)
	}
	if _, err := provider.Unwrap(ctx, "file-missing", []byte{fileWrapVersion, 0}); !errors.Is(err, ErrProviderKeyNotFound) {
		t.Fatalf("unwrap unknown ref = %v", err)
	}
}

func TestFileProviderRejectsTamperedWrap(t *testing.T) {
	t.Parallel()

	provider, err := NewFileProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := provider.ProvisionKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := provider.Wrap(ctx, ref, []byte("key-material"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(wrapped)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := provider.Unwrap(ctx, ref, tampered); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("tampered wrap = %v", err)
	}
	if _, err := provider.Unwrap(ctx, ref, []byte{0x7f}); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("unknown version = %v", err)
	}
	// Wrapped bytes cannot be transplanted across key references.
	other, err := provider.ProvisionKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Unwrap(ctx, other, wrapped); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("transplanted wrap = %v", err)
	}
}

func TestFileProviderConcurrentProvision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := NewFileProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileProvider(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	refs := make([]string, 8)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range refs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provider := first
			if i%2 == 1 {
				provider = second
			}
			ref, err := provider.ProvisionKey(ctx, "")
			refs[i], errs[i] = ref, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, ref := range refs {
		if errs[i] != nil {
			t.Fatalf("provision %d: %v", i, errs[i])
		}
		if seen[ref] {
			t.Fatalf("duplicate ref %q", ref)
		}
		seen[ref] = true
		if _, err := first.Wrap(ctx, ref, []byte("probe")); err != nil {
			t.Fatalf("wrap with %q: %v", ref, err)
		}
	}
}
