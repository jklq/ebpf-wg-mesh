package controlplane

import (
	"context"
	"errors"
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
	archive, image, err := recoveryGuards("", store)
	if err != nil {
		t.Fatal(err)
	}
	if archive(ctx, "source") == nil || image(ctx, "image") == nil {
		t.Fatal("production deletion allowed without recovery configuration")
	}
}
