package source

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestFileSourceArchiveStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store, err := NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("archive payload")
	key, err := ArchiveObjectKey(ArchiveDigest(archive))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, key, archive); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(archive) {
		t.Fatalf("archive = %q, want %q", got, archive)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get after Delete error = %v, want os.ErrNotExist", err)
	}
}

func TestFileSourceArchiveStoreRejectsTraversal(t *testing.T) {
	t.Parallel()

	store, err := NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "../escape", []byte("bad")); err == nil {
		t.Fatal("expected traversal key to be rejected")
	}
}
