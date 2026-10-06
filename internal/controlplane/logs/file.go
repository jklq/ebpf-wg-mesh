package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/logpipeline"
	"github.com/gofrs/flock"
)

// fileStore keeps human-readable JSONL segments. Queries retain only one page;
// the disk budget never becomes an in-memory index. One process owns a directory.
type fileStore struct {
	mu     sync.Mutex
	cfg    config.FileLogConfig
	lock   *flock.Flock
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	closed bool
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
		return nil, errors.New("file log directory already has a writer; use ClickHouse for multiple replicas")
	}
	temps, _ := filepath.Glob(filepath.Join(cfg.Directory, "*.tmp"))
	for _, path := range temps {
		_ = os.Remove(path)
	}
	if err := f.expire(context.Background()); err != nil {
		_ = f.lock.Close()
		return nil, err
	}
	if err := f.trim(); err != nil {
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

// The earliest row expiry in a segment avoids scanning retained files on idle sweeps.
func (f *fileStore) expire(ctx context.Context) error {
	return f.rewriteSegments(ctx, func(path string) bool {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		i := strings.LastIndex(name, "-e")
		if i < 0 {
			return true
		}
		nanos, err := strconv.ParseInt(name[i+2:], 10, 64)
		return err != nil || nanos <= time.Now().UnixNano()
	}, func(r fileRecord) bool { return r.ExpiresAt.After(time.Now()) })
}
func (f *fileStore) close() error {
	f.once.Do(func() { close(f.stop) })
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.lock.Close()
}
func (f *fileStore) ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.cfg.Directory)
	return !f.closed && err == nil && info.IsDir()
}
func (f *fileStore) paths() ([]string, error) {
	return filepath.Glob(filepath.Join(f.cfg.Directory, "*.jsonl"))
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

// Every append is published by rename after fsync. A crash leaves a temporary
// file, never a partial record in the queryable segments.
func (f *fileStore) append(ctx context.Context, records []fileRecord) error {
	if len(records) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("file logs closed")
	}
	// Gap summaries are mutable. Remove their previous versions before publishing
	// replacements, so pagination and changing windows cannot resurrect old totals.
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
	var segment *os.File
	var size int64
	var earliest time.Time
	publish := func() error {
		if segment == nil {
			return nil
		}
		name := segment.Name()
		if err := segment.Sync(); err != nil {
			return err
		}
		if err := segment.Close(); err != nil {
			return err
		}
		segment = nil
		if err := os.Rename(name, fmt.Sprintf("%s-e%d.jsonl", strings.TrimSuffix(name, ".tmp"), earliest.UnixNano())); err != nil {
			return err
		}
		return f.trim()
	}
	defer func() {
		if segment != nil {
			name := segment.Name()
			_ = segment.Close()
			_ = os.Remove(name)
		}
	}()
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
		if segment != nil && size+int64(len(data)) > f.cfg.SegmentBytes {
			if err := publish(); err != nil {
				return err
			}
		}
		if segment == nil {
			segment, err = os.CreateTemp(f.cfg.Directory, fmt.Sprintf("%020d-*.tmp", time.Now().UnixNano()))
			if err != nil {
				return err
			}
			size = 0
			earliest = r.ExpiresAt
		}
		if r.ExpiresAt.Before(earliest) {
			earliest = r.ExpiresAt
		}
		if _, err := segment.Write(data); err != nil {
			return err
		}
		size += int64(len(data))
	}
	if err := publish(); err != nil {
		return err
	}
	return f.trim()
}
func (f *fileStore) trim() error {
	paths, err := f.paths()
	if err != nil {
		return err
	}
	var total int64
	sizes := make([]int64, len(paths))
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		sizes[i] = info.Size()
		total += sizes[i]
	}
	for i, path := range paths {
		if total <= f.cfg.MaxBytes {
			break
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		total -= sizes[i]
	}
	return syncDirectory(f.cfg.Directory)
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (f *fileStore) rewrite(ctx context.Context, keep func(fileRecord) bool) error {
	return f.rewriteSegments(ctx, func(string) bool { return true }, keep)
}
func (f *fileStore) rewriteSegments(ctx context.Context, selectPath func(string) bool, keep func(fileRecord) bool) error {
	paths, err := f.paths()
	if err != nil {
		return err
	}
	for _, path := range paths {
		if !selectPath(path) {
			continue
		}
		tmp, err := os.CreateTemp(f.cfg.Directory, "sweep-*.tmp")
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(tmp)
		count := 0
		changed := false
		err = scanFile(ctx, path, func(r fileRecord) error {
			if !keep(r) {
				changed = true
				return nil
			}
			count++
			return json.NewEncoder(writer).Encode(r)
		})
		if err == nil {
			err = writer.Flush()
		}
		if err == nil && changed {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil && changed {
			if count == 0 {
				err = os.Remove(path)
			} else {
				err = os.Rename(tmp.Name(), path)
			}
		}
		_ = os.Remove(tmp.Name())
		if err != nil {
			return err
		}
	}
	return syncDirectory(f.cfg.Directory)
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
func (s *LogStore) writeFileLines(ctx context.Context, inputs []LogLineInput) error {
	resolved, err := s.resolveProjects(ctx, inputs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	records := make([]fileRecord, 0, len(inputs))
	for _, in := range inputs {
		if strings.TrimSpace(in.ServiceID) == "" {
			continue
		}
		project, expiry, ok := s.attribution(in.ProjectID, in.ExpiresAt, in.ServiceID, resolved, now)
		if !ok {
			continue
		}
		in.ProjectID = project
		in.ExpiresAt = expiry
		if in.ObservedAt.IsZero() {
			in.ObservedAt = now
		}
		in.ObservedAt = in.ObservedAt.UTC()
		if strings.TrimSpace(in.ID) == "" {
			in.ID = logpipeline.SyntheticLineID()
		}
		var truncated bool
		in.Line, truncated = logpipeline.TruncateLine(in.Line)
		in.Truncated = in.Truncated || truncated
		in.Attributes = logpipeline.NormalizeAttributes(in.Attributes)
		in.Stream = normalizeLogStream(in.Stream)
		in.LogType = normalizeLogType(in.LogType)
		in.Stage = normalizeStageName(in.Stage)
		in.Event = logpipeline.NormalizeEvent(in.Event)
		records = append(records, fileRecord{Line: &in, ID: in.ID, ExpiresAt: expiry})
	}
	return s.file.append(ctx, records)
}
func (s *LogStore) writeFileGaps(ctx context.Context, gaps []GapInput) error {
	inputs := make([]LogLineInput, 0, len(gaps))
	for _, g := range gaps {
		inputs = append(inputs, LogLineInput{ServiceID: g.ServiceID, ProjectID: g.ProjectID, ExpiresAt: g.ExpiresAt})
	}
	resolved, err := s.resolveProjects(ctx, inputs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	records := make([]fileRecord, 0, len(gaps))
	for _, g := range gaps {
		if g.ServiceID == "" || g.DroppedCount == 0 {
			continue
		}
		project, expiry, ok := s.attribution(g.ProjectID, g.ExpiresAt, g.ServiceID, resolved, now)
		if !ok {
			continue
		}
		g.ProjectID = project
		g.ExpiresAt = expiry
		if g.WindowStart.IsZero() {
			g.WindowStart = now
		}
		if g.WindowEnd.IsZero() || g.WindowEnd.Before(g.WindowStart) {
			g.WindowEnd = g.WindowStart
		}
		g.LogType = normalizeLogType(g.LogType)
		id := gapIdentity(g.ServiceID, g.AllocationID, g.BuildID, string(g.LogType), g.Stream, g.Reason, g.Reporter, g.SummaryID, g.WindowStart, g.WindowEnd, g.DroppedCount)
		g.Stream = normalizeLogStream(g.Stream)
		g.Reason = logpipeline.NormalizeDropReason(g.Reason)
		records = append(records, fileRecord{Gap: &g, ID: id, ExpiresAt: expiry})
	}
	return s.file.append(ctx, records)
}

// boundedPage keeps the newest limit+1 distinct ordering keys while scanning.
type filePageItem struct {
	record fileRecord
	at     time.Time
	key    string
}

func addFilePage(page []filePageItem, item filePageItem, limit int) []filePageItem {
	i := sort.Search(len(page), func(i int) bool {
		return page[i].at.Before(item.at) || page[i].at.Equal(item.at) && page[i].key <= item.key
	})
	if i < len(page) && page[i].at.Equal(item.at) && page[i].key == item.key {
		page[i] = item
		return page
	}
	if i >= limit {
		return page
	}
	page = append(page, filePageItem{})
	copy(page[i+1:], page[i:])
	page[i] = item
	if len(page) > limit {
		page = page[:limit]
	}
	return page
}
func (f *fileStore) list(ctx context.Context, req *platformv1.ListServiceLogsRequest) (ServiceLogPage, error) {
	var result ServiceLogPage
	cursor, id, err := logpipeline.DecodeCursor(req.GetPageToken())
	if err != nil {
		return result, err
	}
	gapCursor, gapID, err := logpipeline.DecodeCursor(req.GetGapPageToken())
	if err != nil {
		return result, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	limit = min(limit, maxLogQueryLimit)
	f.mu.Lock()
	defer f.mu.Unlock()
	paths, err := f.paths()
	if err != nil {
		return result, err
	}
	var lines, gaps []filePageItem
	now := time.Now()
	search := strings.ToLower(strings.TrimSpace(req.GetSearch()))
	kind := logTypeFromProto(req.GetLogType())
	matches := func(service, allocation, build string, t LogType, isGap bool) bool {
		return service == req.GetServiceId() && (req.GetAllocationId() == "" || allocation == req.GetAllocationId() || isGap && allocation == "") && (req.GetBuildId() == "" || build == req.GetBuildId() || isGap && build == "") && (kind == "" || kind == t)
	}
	older := func(at time.Time, key string, cursor time.Time, id string) bool {
		return cursor.IsZero() || at.Before(cursor) || at.Equal(cursor) && key < id
	}
	for _, path := range paths {
		err = scanFile(ctx, path, func(r fileRecord) error {
			if !r.ExpiresAt.After(now) {
				return nil
			}
			if l := r.Line; l != nil {
				if !matches(l.ServiceID, l.AllocationID, l.BuildID, l.LogType, false) || !older(l.ObservedAt, r.ID, cursor, id) || search != "" && !strings.Contains(strings.ToLower(l.Line), search) {
					return nil
				}
				if req.GetStartTime() != nil && l.ObservedAt.Before(req.GetStartTime().AsTime()) || req.GetEndTime() != nil && l.ObservedAt.After(req.GetEndTime().AsTime()) {
					return nil
				}
				lines = addFilePage(lines, filePageItem{r, l.ObservedAt, r.ID}, limit+1)
			}
			if g := r.Gap; g != nil {
				if !matches(g.ServiceID, g.AllocationID, g.BuildID, g.LogType, true) || !older(g.WindowStart, r.ID, gapCursor, gapID) {
					return nil
				}
				if req.GetStartTime() != nil && g.WindowEnd.Before(req.GetStartTime().AsTime()) || req.GetEndTime() != nil && g.WindowStart.After(req.GetEndTime().AsTime()) {
					return nil
				}
				gaps = addFilePage(gaps, filePageItem{r, g.WindowStart, r.ID}, maxLogGapResults+1)
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	if len(lines) > limit {
		last := lines[limit-1]
		result.NextPageToken = logpipeline.EncodeCursor(last.at, last.key)
		lines = lines[:limit]
	}
	for _, item := range lines {
		l := item.record.Line
		result.Lines = append(result.Lines, ServiceLog{ObservedAt: l.ObservedAt, ProjectID: l.ProjectID, EnvironmentID: l.EnvironmentID, ServiceID: l.ServiceID, AllocationID: l.AllocationID, AgentID: l.AgentID, Stream: l.Stream, RolloutGeneration: l.RolloutGeneration, Sequence: l.Sequence, LineID: l.ID, Line: l.Line, LogType: string(l.LogType), BuildID: l.BuildID, Stage: l.Stage, Event: l.Event, Attributes: l.Attributes, Truncated: l.Truncated})
	}
	if len(gaps) > maxLogGapResults {
		last := gaps[maxLogGapResults-1]
		result.NextGapPageToken = logpipeline.EncodeCursor(last.at, last.key)
		gaps = gaps[:maxLogGapResults]
	}
	for _, item := range gaps {
		g := item.record.Gap
		result.Gaps = append(result.Gaps, ServiceLogGap{AllocationID: g.AllocationID, BuildID: g.BuildID, LogType: string(g.LogType), Stream: g.Stream, DroppedCount: g.DroppedCount, Reason: g.Reason, WindowStart: g.WindowStart, WindowEnd: g.WindowEnd})
	}
	return result, nil
}
