package volumestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// directoryBackend keeps each volume as <root>/data. It measures usage but
// cannot stop a workload from writing past the volume's size.
type directoryBackend struct{}

func (directoryBackend) ensure(root string, _ int64) error {
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		return fmt.Errorf("create volume directory: %w", err)
	}
	return nil
}

func (directoryBackend) dataPath(root string) (string, error) {
	path := filepath.Join(root, "data")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotProvisioned
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("volume data path %s is not a directory", path)
	}
	return path, nil
}

func (directoryBackend) usage(root string, sizeBytes int64) (int64, int64, int64, error) {
	var used int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			used += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("measure volume usage: %w", err)
	}
	capacity := sizeBytes
	if capacity <= 0 {
		return used, 0, 0, nil
	}
	return used, capacity, max(capacity-used, 0), nil
}

func (directoryBackend) destroy(root string) error {
	return os.RemoveAll(root)
}
