package source

import (
	"bytes"
	"context"
	"testing"
)

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
