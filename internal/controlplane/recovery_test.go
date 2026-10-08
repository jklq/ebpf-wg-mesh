package controlplane

import (
	"context"
	"errors"
	"os"
	"testing"

	"ebof-wg-mesh/internal/controlplane/source"
)

func TestActiveArchiveDeletionRequiresProtectedCopy(t *testing.T) {
	ctx := context.Background()
	store, err := source.NewFileArchiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	a := protectedArchives{ArchiveStore: store, protect: func(context.Context, string) error {
		called = true
		return errors.New("independent storage unavailable")
	}}
	if err := a.Delete(ctx, "source"); err == nil || !called {
		t.Fatal("archive deletion bypassed independent storage")
	}
	archive, image, err := recoveryGuards("", store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if archive(ctx, "source") == nil || image(ctx, "image") == nil {
		t.Fatal("production deletion allowed without recovery configuration")
	}
}

func TestProtectedArchivesPreserveActualReadiness(t *testing.T) {
	root := t.TempDir()
	store, err := source.NewFileArchiveStore(root)
	if err != nil {
		t.Fatal(err)
	}
	archives := protectedArchives{ArchiveStore: store}
	if !archives.Ready() {
		t.Fatal("healthy production archive wrapper lost native readiness")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if archives.Ready() {
		t.Fatal("production archive wrapper accepted unavailable native storage")
	}
}
