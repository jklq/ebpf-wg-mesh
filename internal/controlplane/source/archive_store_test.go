package source

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestFileSourceArchiveStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store, err := NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("archive payload")
	digest := ArchiveDigest(archive)
	key, err := ArchiveObjectKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, key, bytes.NewReader(archive), int64(len(archive)), digest); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(archive)) || meta.Digest != digest {
		t.Fatalf("stat = %+v, want size %d digest %s", meta, len(archive), digest)
	}
	reader, opened, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(archive) || opened.Digest != digest {
		t.Fatalf("open = %q %+v, want %q", got, opened, archive)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(ctx, key); !IsArchiveNotFound(err) {
		t.Fatalf("Open after Delete error = %v, want archive not found", err)
	}
}

func TestFileSourceArchiveStoreRejectsTraversal(t *testing.T) {
	t.Parallel()

	store, err := NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("bad")
	if err := store.Put(context.Background(), "../escape", bytes.NewReader(archive), int64(len(archive)), ArchiveDigest(archive)); err == nil {
		t.Fatal("expected traversal key to be rejected")
	}
	ctx := context.Background()
	if _, _, err := store.Open(ctx, "../escape"); err == nil {
		t.Fatal("expected Open traversal key to be rejected")
	}
	if _, err := store.Stat(ctx, "../escape"); err == nil {
		t.Fatal("expected Stat traversal key to be rejected")
	}
	if _, err := store.ReadRange(ctx, "../escape", 0, 4); err == nil {
		t.Fatal("expected ReadRange traversal key to be rejected")
	}
	if err := store.Delete(ctx, "../escape"); err == nil {
		t.Fatal("expected Delete traversal key to be rejected")
	}
}
