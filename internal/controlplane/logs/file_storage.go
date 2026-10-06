package logs

import (
	"bufio"
	"bytes"
	"context"
	"ebof-wg-mesh/internal/config"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fileStore keeps human-readable JSONL segments, with no in-memory row index.
// Metadata is proportional to segments; queries retain one bounded page.
// One process owns a directory. All writes and reads cross the same mutex.
type fileStore struct {
	mu       sync.Mutex
	cfg      config.FileLogConfig
	lock     *flock.Flock
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	closed   bool
	active   *os.File
	segments []fileSegment
}
type fileSegment struct {
	path   string
	size   int64
	expiry time.Time
}
type fileRecord struct {
	Line      *LogLineInput `json:"line,omitempty"`
	Gap       *GapInput     `json:"gap,omitempty"`
	ID        string        `json:"id"`
	ExpiresAt time.Time     `json:"expires_at"`
}

func openFileStore(cfg config.FileLogConfig) (*fileStore, error) {
	if strings.TrimSpace(cfg.Directory) == "" {
		return nil, errors.New("file log directory is required")
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 64 << 20
	}
	if cfg.SegmentBytes == 0 {
		cfg.SegmentBytes = 1 << 20
	}
	if cfg.SegmentBytes < 256<<10 || cfg.MaxBytes < cfg.SegmentBytes {
		return nil, errors.New("file log budget requires maxBytes >= segmentBytes >= 256 KiB")
	}
	if err := os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, err
	}
	f := &fileStore{cfg: cfg, lock: flock.New(filepath.Join(cfg.Directory, ".lock")), stop: make(chan struct{}), done: make(chan struct{})}
	ok, err := f.lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		_ = f.lock.Close()
		return nil, errors.New("file log directory already has a writer; use ClickHouse for multiple replicas")
	}
	err = func() error {
		temps, err := filepath.Glob(filepath.Join(cfg.Directory, "*.tmp"))
		if err != nil {
			return err
		}
		for _, path := range temps {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		paths, err := filepath.Glob(filepath.Join(cfg.Directory, "*.jsonl"))
		if err != nil {
			return err
		}
		if len(paths) > 0 {
			if err := repairFileTail(paths[len(paths)-1]); err != nil {
				return err
			}
		}
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			segment := fileSegment{path: path, size: info.Size()}
			if err := scanFile(context.Background(), path, func(r fileRecord) error { segment.expiry = earliestExpiry(segment.expiry, r.ExpiresAt); return nil }); err != nil {
				return err
			}
			f.segments = append(f.segments, segment)
		}
		if err := f.expire(context.Background()); err != nil {
			return err
		}
		return f.trim()
	}()
	if err != nil {
		_ = f.lock.Close()
		return nil, err
	}
	go func() {
		defer close(f.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-ticker.C:
				f.mu.Lock()
				err := f.expire(context.Background())
				f.mu.Unlock()
				if err != nil {
					slog.Warn("sweep file log retention", "error", err)
				}
			}
		}
	}()
	return f, nil
}
func earliestExpiry(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// Only the active segment can have an incomplete append after a crash. Records
// with a newline were fully written. Drop the unacknowledged partial tail.
func repairFileTail(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	size := info.Size()
	data := make([]byte, min(size, 256<<10))
	if _, err := file.ReadAt(data, size-int64(len(data))); err != nil {
		return err
	}
	if data[len(data)-1] == '\n' {
		return nil
	}
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 && size > int64(len(data)) {
		return errors.New("file log has an oversized partial tail")
	}
	if err := file.Truncate(size - int64(len(data)) + int64(cut+1)); err != nil {
		return err
	}
	return file.Sync()
}
func (f *fileStore) close() error {
	f.once.Do(func() { close(f.stop) })
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return errors.Join(f.closeActive(), f.lock.Close())
}
func (f *fileStore) ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.cfg.Directory)
	return !f.closed && err == nil && info.IsDir()
}
func (f *fileStore) paths() ([]string, error) {
	paths := make([]string, len(f.segments))
	for i, s := range f.segments {
		paths[i] = s.path
	}
	return paths, nil
}
func scanFile(ctx context.Context, path string, visit func(fileRecord) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec fileRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return fmt.Errorf("decode file log %s: %w", path, err)
		}
		if err := visit(rec); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func (f *fileStore) closeActive() error {
	if f.active == nil {
		return nil
	}
	err := f.active.Sync()
	closeErr := f.active.Close()
	f.active = nil
	return errors.Join(err, closeErr)
}
func (f *fileStore) append(ctx context.Context, records []fileRecord) error {
	if len(records) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("file logs closed")
	}
	replacements := map[string]bool{}
	for _, r := range records {
		if r.Gap != nil {
			replacements[r.ID] = true
		}
	}
	if len(replacements) > 0 {
		if err := f.rewrite(ctx, func(r fileRecord) bool { return r.Gap == nil || !replacements[r.ID] }); err != nil {
			return err
		}
	}
	changedDirectory := false
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.ExpiresAt.After(time.Now()) {
			continue
		}
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if int64(len(data)) > f.cfg.SegmentBytes {
			return errors.New("file log record exceeds segment budget")
		}
		if len(f.segments) == 0 || f.segments[len(f.segments)-1].size+int64(len(data)) > f.cfg.SegmentBytes {
			if err := f.closeActive(); err != nil {
				return err
			}
			if err := f.trim(); err != nil {
				return err
			}
			file, err := os.CreateTemp(f.cfg.Directory, fmt.Sprintf("%020d-*.jsonl", time.Now().UnixNano()))
			if err != nil {
				return err
			}
			f.active = file
			f.segments = append(f.segments, fileSegment{path: file.Name()})
			changedDirectory = true
		}
		segment := &f.segments[len(f.segments)-1]
		if f.active == nil {
			f.active, err = os.OpenFile(segment.path, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				return err
			}
		}
		n, err := f.active.Write(data)
		if err != nil || n != len(data) {
			truncateErr := f.active.Truncate(segment.size)
			// A newly created file has a regular write offset. Reopen with
			// O_APPEND on retry so a partial write cannot leave a sparse hole.
			return errors.Join(err, io.ErrShortWrite, truncateErr, f.closeActive())
		}
		segment.size += int64(n)
		segment.expiry = earliestExpiry(segment.expiry, r.ExpiresAt)
	}
	if f.active != nil {
		if err := f.active.Sync(); err != nil {
			return err
		}
	}
	// A write temporarily needs at most one extra segment before removing the
	// oldest segment. A completed append always satisfies the global byte budget.
	if err := f.trim(); err != nil {
		return err
	}
	if changedDirectory {
		return syncDirectory(f.cfg.Directory)
	}
	return nil
}
func (f *fileStore) trim() error {
	var total int64
	for _, s := range f.segments {
		total += s.size
	}
	removed := 0
	for _, s := range f.segments {
		if total <= f.cfg.MaxBytes {
			break
		}
		if f.active != nil && f.active.Name() == s.path {
			if err := f.closeActive(); err != nil {
				return err
			}
		}
		if err := os.Remove(s.path); err != nil {
			return err
		}
		total -= s.size
		removed++
	}
	if removed > 0 {
		f.segments = f.segments[removed:]
		return syncDirectory(f.cfg.Directory)
	}
	return nil
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (f *fileStore) expire(ctx context.Context) error {
	return f.rewriteSegments(ctx, func(s fileSegment) bool { return !s.expiry.After(time.Now()) }, func(r fileRecord) bool { return r.ExpiresAt.After(time.Now()) })
}
func (f *fileStore) rewrite(ctx context.Context, keep func(fileRecord) bool) error {
	return f.rewriteSegments(ctx, func(fileSegment) bool { return true }, keep)
}
func (f *fileStore) rewriteSegments(ctx context.Context, selected func(fileSegment) bool, keep func(fileRecord) bool) error {
	changed := false
	for i := 0; i < len(f.segments); i++ {
		s := f.segments[i]
		if !selected(s) {
			continue
		}
		if f.active != nil && f.active.Name() == s.path {
			if err := f.closeActive(); err != nil {
				return err
			}
		}
		tmp, err := os.CreateTemp(f.cfg.Directory, "sweep-*.tmp")
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(tmp)
		count := 0
		modified := false
		var expiry time.Time
		err = scanFile(ctx, s.path, func(r fileRecord) error {
			if !keep(r) {
				modified = true
				return nil
			}
			count++
			expiry = earliestExpiry(expiry, r.ExpiresAt)
			return json.NewEncoder(writer).Encode(r)
		})
		if err == nil {
			err = writer.Flush()
		}
		if err == nil && modified {
			err = tmp.Sync()
		}
		info, statErr := tmp.Stat()
		if err == nil {
			err = statErr
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil && modified {
			changed = true
			if count == 0 {
				err = os.Remove(s.path)
				if err == nil {
					f.segments = append(f.segments[:i], f.segments[i+1:]...)
					i--
				}
			} else {
				err = os.Rename(tmp.Name(), s.path)
				if err == nil {
					f.segments[i].size = info.Size()
					f.segments[i].expiry = expiry
				}
			}
		}
		_ = os.Remove(tmp.Name())
		if err != nil {
			return err
		}
		if !modified {
			f.segments[i].expiry = expiry
		}
	}
	if changed {
		return syncDirectory(f.cfg.Directory)
	}
	return nil
}
func (f *fileStore) purge(ctx context.Context, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rewrite(ctx, func(r fileRecord) bool {
		if r.Line != nil {
			return r.Line.ProjectID != projectID
		}
		return r.Gap == nil || r.Gap.ProjectID != projectID
	})
}
