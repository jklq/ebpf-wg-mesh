package continuity

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func copyConfig(source, target string, replace func(string) string) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		if !included(d.Name()) {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || filepath.Clean(link) == ".." || strings.HasPrefix(filepath.Clean(link), "../") {
				return fmt.Errorf("configuration link escapes its snapshot")
			}
			return os.Symlink(link, dst)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("configuration snapshot contains a nonregular file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if _, err = f.Write([]byte(replace(string(b)))); err != nil {
			f.Close()
			return err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

func snapshotConfig(dir string, l Launch) (string, error) {
	inputs := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inputs, 0700); err != nil {
		return "", err
	}
	var bytes int64
	if err := filepath.WalkDir(l.ConfigDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := storageAvailable(dir, 2*bytes+(1<<20)); err != nil {
		return "", err
	}
	path := filepath.Join(inputs, l.ID)
	if digest, err := directoryDigest(path); err == nil {
		if digest != l.ConfigDigest {
			return "", fmt.Errorf("immutable boot inputs have changed")
		}
		return path, nil
	}
	stage, err := os.MkdirTemp(inputs, ".snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = copyConfig(l.ConfigDir, stage, func(v string) string { return v }); err != nil {
		return "", err
	}
	if digest, err := directoryDigest(stage); err != nil || digest != l.ConfigDigest {
		return "", fmt.Errorf("configuration changed during snapshot; serving generation retained")
	}
	// Every directory entry and file is durable before publishing the boot view.
	if err = filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	}); err != nil {
		return "", err
	}
	if err = os.Rename(stage, path); err != nil {
		return "", err
	}
	f, err := os.Open(inputs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return path, f.Sync()
}
