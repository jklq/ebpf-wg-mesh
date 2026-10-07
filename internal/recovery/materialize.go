package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Materialize reads the exact protected version, checks its digest and decrypts
// secret material using the independent recovery key. No active vault is needed.
// The caller selects the destination; dependency IDs are never filesystem paths.
func (s Service) Materialize(ctx context.Context, d Dependency, target string, secret bool) error {
	if len(d.Objects) != 1 || !filepath.IsAbs(target) {
		return fmt.Errorf("single-object dependency and absolute destination required")
	}
	if err := s.verifyObject(ctx, d.Objects[0], d.Objects[0].RetainUntil, true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".materialize-")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	if secret {
		b, err := s.bundle(ctx, d)
		if err != nil {
			return err
		}
		defer clear(b)
		if err := os.WriteFile(name, b, 0600); err != nil {
			return err
		}
	} else {
		if err := s.Storage.Get(ctx, d.Objects[0], name); err != nil {
			return err
		}
		digest, size, err := FileDigest(name)
		if err != nil || digest != d.Objects[0].Digest || size != d.Objects[0].Size {
			return fmt.Errorf("materialized dependency digest mismatch")
		}
	}
	if err := os.Chmod(name, 0600); err != nil {
		return err
	}
	f, err = os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	if err := os.Rename(name, target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
