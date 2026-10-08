package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/recovery"
)

// protectedArchives lets active GC proceed only after an independent copy is
// protected. GC never receives recovery-storage deletion capabilities.
type protectedArchives struct {
	source.ArchiveStore
	protect func(context.Context, string) error
}

func (a protectedArchives) Delete(ctx context.Context, key string) error {
	if err := a.protect(ctx, key); err != nil {
		return err
	}
	return a.ArchiveStore.Delete(ctx, key)
}

func recoveryGuards(path string, archives source.ArchiveStore, db *sql.DB) (func(context.Context, string) error, func(context.Context, string) error, error) {
	if path == "" {
		deny := func(context.Context, string) error {
			return fmt.Errorf("production artifact deletion requires independent recovery configuration")
		}
		return deny, deny, nil
	}
	c, s, err := recovery.Load(path)
	if err != nil {
		return nil, nil, err
	}
	archive := func(ctx context.Context, key string) error {
		digest, err := source.DigestFromObjectKey(key)
		if err != nil {
			return err
		}
		return s.ProtectArchive(ctx, key, digest, func(w io.Writer) error {
			meta, err := archives.Stat(ctx, key)
			if err != nil {
				return err
			}
			for offset := int64(0); offset < meta.Size; {
				n := int(min(int64(1<<20), meta.Size-offset))
				b, err := archives.ReadRange(ctx, key, offset, n)
				if err != nil {
					return err
				}
				if len(b) != n {
					return io.ErrUnexpectedEOF
				}
				if _, err := w.Write(b); err != nil {
					return err
				}
				offset += int64(n)
			}
			return nil
		})
	}
	image := func(ctx context.Context, ref string) error {
		images, releaseAccess, err := recovery.ImageAccess(ctx, db, c.KeyringFile, c.Images, ref, false)
		if err != nil {
			return err
		}
		defer releaseAccess()
		_, err = s.ProtectImage(ctx, images, ref)
		return err
	}
	return archive, image, nil
}
