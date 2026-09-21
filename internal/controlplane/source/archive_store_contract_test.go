package source

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func runArchiveStoreContractSuite(t *testing.T, name string, newStore func(t *testing.T) ArchiveStore) {
	t.Helper()
	t.Run(name+"/round-trip streams content", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := make([]byte, 2<<20)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
			t.Fatal(err)
		}
		meta, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Size != int64(len(payload)) || meta.Digest != digest {
			t.Fatalf("stat = %+v, want size %d digest %s", meta, len(payload), digest)
		}
		reader, opened, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if !bytes.Equal(got, payload) || opened.Size != int64(len(payload)) || opened.Digest != digest {
			t.Fatal("streamed content does not match stored content")
		}
	})

	t.Run(name+"/put verifies size and digest", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("verified-content")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		wrongDigest := ArchiveDigest([]byte("other-content"))
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), wrongDigest); err == nil || !IsArchiveCorrupt(err) {
			t.Fatalf("wrong digest error = %v, want corrupt", err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))-1, digest); err == nil || !IsArchiveCorrupt(err) {
			t.Fatalf("short size error = %v, want corrupt", err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))+1, digest); err == nil || !IsArchiveCorrupt(err) {
			t.Fatalf("long size error = %v, want corrupt", err)
		}
		otherKey, err := ArchiveObjectKey(wrongDigest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, otherKey, bytes.NewReader(payload), int64(len(payload)), digest); err == nil || !IsArchiveCorrupt(err) {
			t.Fatalf("key mismatch error = %v, want corrupt", err)
		}
		if _, err := store.Stat(ctx, key); !IsArchiveNotFound(err) {
			t.Fatalf("stat after failed puts = %v, want not found", err)
		}
	})

	t.Run(name+"/partial upload leaves no object", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("partial-content-that-will-be-cut")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, io.MultiReader(bytes.NewReader(payload[:8]), failingReader{err: errors.New("connection reset")}), int64(len(payload)), digest); err == nil {
			t.Fatal("expected partial upload to fail")
		}
		if _, err := store.Stat(ctx, key); !IsArchiveNotFound(err) {
			t.Fatalf("stat after partial upload = %v, want not found", err)
		}
	})

	t.Run(name+"/duplicate puts converge", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("convergent-content")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("duplicate put %d: %v", i, err)
			}
		}
		meta, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Size != int64(len(payload)) || meta.Digest != digest {
			t.Fatalf("stat = %+v", meta)
		}
	})

	t.Run(name+"/conflicting size is corrupt", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		first := []byte("first-content")
		digest := ArchiveDigest(first)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(first), int64(len(first)), digest); err != nil {
			t.Fatal(err)
		}
		second := []byte("first-content-padded")
		if err := store.Put(ctx, key, bytes.NewReader(second), int64(len(second)), ArchiveDigest(second)); err == nil {
			t.Fatal("expected conflicting put to fail")
		}
		meta, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Size != int64(len(first)) {
			t.Fatalf("original object changed: %+v", meta)
		}
	})

	t.Run(name+"/range reads", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("0123456789abcdef")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
			t.Fatal(err)
		}
		chunk, err := store.ReadRange(ctx, key, 4, 4)
		if err != nil || string(chunk) != "4567" {
			t.Fatalf("middle range = %q %v", chunk, err)
		}
		chunk, err = store.ReadRange(ctx, key, 0, 1<<20)
		if err != nil || string(chunk) != string(payload) {
			t.Fatalf("clamped range = %q %v", chunk, err)
		}
		chunk, err = store.ReadRange(ctx, key, int64(len(payload)), 8)
		if err != nil || len(chunk) != 0 {
			t.Fatalf("past-end range = %q %v", chunk, err)
		}
		if _, err := store.ReadRange(ctx, key, -1, 4); err == nil {
			t.Fatal("expected negative offset to fail")
		}
		if _, err := store.ReadRange(ctx, key, 0, 0); err == nil {
			t.Fatal("expected zero limit to fail")
		}
	})

	t.Run(name+"/missing object errors", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		key, err := ArchiveObjectKey(ArchiveDigest([]byte("absent")))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Stat(ctx, key); !IsArchiveNotFound(err) {
			t.Fatalf("stat error = %v, want not found", err)
		}
		if _, _, err := store.Open(ctx, key); !IsArchiveNotFound(err) {
			t.Fatalf("open error = %v, want not found", err)
		}
		if _, err := store.ReadRange(ctx, key, 0, 8); !IsArchiveNotFound(err) {
			t.Fatalf("range error = %v, want not found", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
	})

	t.Run(name+"/delete removes objects", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("delete-me")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: %v", err)
		}
		if _, err := store.Stat(ctx, key); !IsArchiveNotFound(err) {
			t.Fatalf("stat after delete = %v, want not found", err)
		}
	})

	t.Run(name+"/rejects oversized objects", func(t *testing.T) {
		t.Parallel()
		store := newStore(t)
		ctx := context.Background()
		payload := []byte("x")
		digest := ArchiveDigest(payload)
		key, err := ArchiveObjectKey(digest)
		if err != nil {
			t.Fatal(err)
		}
		err = store.Put(ctx, key, bytes.NewReader(payload), MaxArchiveCompressedBytes+1, digest)
		if err == nil || !errors.Is(err, ErrArchiveTooLarge) {
			t.Fatalf("oversized error = %v, want too large", err)
		}
	})
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestFileArchiveStoreContract(t *testing.T) {
	t.Parallel()
	runArchiveStoreContractSuite(t, "file", func(t *testing.T) ArchiveStore {
		t.Helper()
		store, err := NewFileArchiveStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestS3ArchiveStoreContract(t *testing.T) {
	t.Parallel()
	runArchiveStoreContractSuite(t, "s3", func(t *testing.T) ArchiveStore {
		t.Helper()
		fake := newFakeS3(t, "contract-bucket")
		store, err := NewS3ArchiveStore(fake.config())
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestDigestFromObjectKeyRoundTrip(t *testing.T) {
	t.Parallel()
	digest := ArchiveDigest([]byte("key-material"))
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DigestFromObjectKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if back != digest {
		t.Fatalf("digest = %q, want %q", back, digest)
	}
	for _, bad := range []string{"", "sha256/ab", "sha256/ab/cd.tgz", "../escape", "sha256/" + strings.Repeat("0", 64) + ".tgz", "sha256/ff/" + strings.Repeat("0", 64) + ".tgz"} {
		if _, err := DigestFromObjectKey(bad); err == nil {
			t.Fatalf("key %q unexpectedly accepted", bad)
		}
	}
}
