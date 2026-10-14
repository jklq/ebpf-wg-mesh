package builder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Each cache scope owns an import directory. Exports are staged in the disposable
// workspace, then replace it after success. Fresh exports drop old local-cache
// blobs instead of letting BuildKit's local backend grow indefinitely.
type buildCache struct {
	dir      string
	lock     *flock.Flock
	maxBytes int64
}

func openBuildCache(ctx context.Context, workDir string, spec ExecutionSpec) (*buildCache, error) {
	dir, err := resolveExecutionCacheDir(workDir, spec)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(dir + ".lock")
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !locked {
		_ = lock.Close()
		return nil, ctx.Err()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &buildCache{dir: dir, lock: lock, maxBytes: spec.Cache.MaxBytes}, nil
}

func (c *buildCache) close() { _ = c.lock.Close() }

func (c *buildCache) retain(ctx context.Context, exported string) error {
	// An interrupted exporter can produce no index. Preserve the previous
	// usable cache rather than replacing it with an empty directory.
	if _, err := os.Stat(filepath.Join(exported, "index.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := filepath.WalkDir(exported, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("cache export contains a non-regular file: %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	newBytes, err := dirBytes(exported)
	if err != nil {
		return err
	}
	if newBytes > c.maxBytes {
		return nil
	}
	root := filepath.Dir(c.dir)
	gc := flock.New(filepath.Join(root, "gc.lock"))
	defer gc.Close()
	locked, err := gc.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return ctx.Err()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type candidate struct {
		path     string
		bytes    int64
		modified time.Time
	}
	var candidates []candidate
	total := newBytes
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !entry.IsDir() || !(strings.HasPrefix(entry.Name(), "service-sha256-") || strings.HasPrefix(entry.Name(), "cache-sha256-")) || path == c.dir {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes, err := dirBytes(path)
		if err != nil {
			return err
		}
		total += bytes
		candidates = append(candidates, candidate{path: path, bytes: bytes, modified: info.ModTime()})
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return a.modified.Compare(b.modified) })
	for _, candidate := range candidates {
		if total <= c.maxBytes {
			break
		}
		lock := flock.New(candidate.path + ".lock")
		locked, err := lock.TryLock()
		if err != nil {
			_ = lock.Close()
			return err
		}
		if locked {
			err = os.RemoveAll(candidate.path)
			if err == nil {
				total -= candidate.bytes
			}
		}
		_ = lock.Close()
		if err != nil {
			return err
		}
	}
	// In-flight caches stay protected. If they consume the available budget,
	// skip this export; the successfully pushed image remains usable.
	if total > c.maxBytes {
		return nil
	}
	if err := os.RemoveAll(c.dir); err != nil {
		return err
	}
	return os.Rename(exported, c.dir)
}
