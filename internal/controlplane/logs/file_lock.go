package logs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The process mutex covers flock ownership, since an OS file lock by itself
// does not serialize goroutines sharing its descriptor. Metadata is refreshed
// under the same lock; unchanged segments do not need to be decoded again.
func (f *fileStore) acquire(ctx context.Context) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("file logs closed")
	}
	ok, err := f.lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil || !ok {
		f.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("file log lock unavailable")
	}
	if err = f.refresh(ctx); err != nil {
		f.lock.Unlock()
		f.mu.Unlock()
		return err
	}
	return nil
}

func (f *fileStore) release() { f.closeActive(); f.lock.Unlock(); f.mu.Unlock() }

func (f *fileStore) refresh(ctx context.Context) error {
	previous := map[string]fileSegment{}
	for _, s := range f.segments {
		previous[s.path] = s
	}
	paths, err := filepath.Glob(filepath.Join(f.cfg.Directory, "*.jsonl"))
	if err != nil {
		return err
	}
	var segments []fileSegment
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		s, ok := previous[path]
		if !ok || s.size != info.Size() || !s.modified.Equal(info.ModTime()) {
			// No writer can append while this lock is held. Repair every changed
			// segment, including an interrupted writer from another generation.
			if err = repairFileTail(path); err != nil {
				return err
			}
			info, err = os.Stat(path)
			if err != nil {
				return err
			}
			s = fileSegment{path: path, size: info.Size(), modified: info.ModTime()}
			if err = scanFile(ctx, path, func(r fileRecord) error { s.expiry = earliestExpiry(s.expiry, r.ExpiresAt); return nil }); err != nil {
				return err
			}
		}
		segments = append(segments, s)
	}
	f.segments = segments
	return nil
}
func (f *fileStore) sweep(ctx context.Context) error {
	if err := f.acquire(ctx); err != nil {
		return err
	}
	defer f.release()
	return f.expire(ctx)
}
