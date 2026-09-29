package logpipeline

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	defaultSpoolMaxBytes        = 256 << 20
	defaultSpoolMaxSegmentBytes = 8 << 20
	spoolFileMode               = 0o600
	spoolDirMode                = 0o700

	spoolSegmentPattern = "seg-%010d.log"
	spoolCursorFile     = "cursor.json"
)

// Record is one spooled payload. Key attributes drop accounting;
// ID is the stable identity for server-side deduplication.
type Record struct {
	Key        string
	ID         string
	ObservedAt time.Time
	Payload    []byte
}

// Cursor is a read position: the next unread byte offset in a segment.
type Cursor struct {
	Segment uint64
	Offset  int64
}

// SpoolStats reports spool health and lifetime drop counters.
type SpoolStats struct {
	Segments       int
	Records        int64
	Bytes          int64
	PendingRecords int64
	DroppedRecords uint64
	DroppedBytes   uint64
	CorruptRecords uint64
}

// SpoolConfig bounds one disk-backed FIFO.
type SpoolConfig struct {
	// Dir holds segment and cursor files. Required.
	Dir string
	// MaxBytes caps total spool size; oldest segments are evicted past
	// it and their unshipped records count as drops. Defaults to 256 MiB.
	MaxBytes int64
	// MaxSegmentBytes rotates the active segment past this size.
	// Defaults to 8 MiB.
	MaxSegmentBytes int64
	// SyncWrites fsyncs every append. Disable only in tests.
	SyncWrites bool
	// RejectOnFull fails Append with ErrSpoolFull instead of evicting
	// unshipped records, for journals where an accepted batch must
	// survive outages.
	RejectOnFull bool
	// Retention keeps committed sealed segments this long past their
	// newest record for reconnect replay. Zero collects them immediately.
	Retention time.Duration
}

// Spool is a bounded disk-backed FIFO queue. Appends are durable with
// SyncWrites; reads replay from the last commit. Past the byte cap the
// oldest segments are evicted and their unshipped records counted per key.
type Spool struct {
	mu           sync.Mutex
	dir          string
	maxBytes     int64
	maxSeg       int64
	rejectOnFull bool
	sync         bool
	retention    time.Duration

	activeID   uint64
	active     *os.File
	activeSize int64
	nextSeg    uint64

	segments []segmentInfo
	cursor   Cursor

	// inflight pins the segments spanned by the outstanding Read batch.
	inflight map[uint64]struct{}

	records        int64
	bytes          int64
	evicted        map[string]uint64
	corruptDrops   map[string]uint64
	corruptIDs     map[string]string
	droppedRecords uint64
	droppedBytes   uint64
	corrupt        uint64
	closed         bool
}

type segmentInfo struct {
	id             uint64
	size           int64
	records        int64
	newestObserved time.Time
	pins           int
}

// OpenSpool opens or creates the spool in cfg.Dir, truncating torn tails
// and compacting corrupt records out as attributable drops.
func OpenSpool(cfg SpoolConfig) (*Spool, error) {
	if cfg.Dir == "" {
		return nil, errors.New("log spool directory is required")
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultSpoolMaxBytes
	}
	maxSeg := cfg.MaxSegmentBytes
	if maxSeg <= 0 {
		maxSeg = defaultSpoolMaxSegmentBytes
	}
	// Eviction needs a sealed segment, so a segment can never exceed the cap.
	if maxSeg > maxBytes {
		maxSeg = maxBytes
	}
	if err := os.MkdirAll(cfg.Dir, spoolDirMode); err != nil {
		return nil, fmt.Errorf("create log spool dir: %w", err)
	}
	s := &Spool{
		dir:          cfg.Dir,
		maxBytes:     maxBytes,
		rejectOnFull: cfg.RejectOnFull,
		maxSeg:       maxSeg,
		sync:         cfg.SyncWrites,
		retention:    cfg.Retention,
		evicted:      make(map[string]uint64),
		corruptDrops: make(map[string]uint64),
		corruptIDs:   make(map[string]string),
		inflight:     make(map[uint64]struct{}),
	}
	// Load first so recovery-time losses are not counted twice.
	s.loadDrops()
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Spool) recover() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("list log spool dir: %w", err)
	}
	var ids []uint64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var id uint64
		if _, err := fmt.Sscanf(entry.Name(), spoolSegmentPattern, &id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	cursor, err := s.loadCursor()
	if err != nil {
		return err
	}
	s.cursor = cursor
	for _, id := range ids {
		cursorOff := int64(-1)
		if cursor.Segment == id {
			cursorOff = cursor.Offset
		}
		size, records, newest, mapped, err := s.compactSegment(id, cursorOff)
		if err != nil {
			return err
		}
		if cursorOff >= 0 {
			cursor.Offset = mapped
			s.cursor = cursor
		}
		s.segments = append(s.segments, segmentInfo{id: id, size: size, records: records, newestObserved: newest})
		s.records += records
		s.bytes += size
		if id >= s.nextSeg {
			s.nextSeg = id + 1
		}
	}
	s.cursor = s.clampCursor(cursor)
	if len(s.segments) == 0 {
		if err := s.rotateLocked(); err != nil {
			return err
		}
	} else {
		last := s.segments[len(s.segments)-1].id
		f, err := os.OpenFile(s.segmentPath(last), os.O_WRONLY|os.O_APPEND, spoolFileMode)
		if err != nil {
			return fmt.Errorf("open active spool segment: %w", err)
		}
		s.activeID = last
		s.active = f
		s.activeSize = s.segments[len(s.segments)-1].size
	}
	return nil
}

// compactSegment drops corrupt records and a torn tail from one segment,
// remapping cursorOff into the compacted layout. The remap must never let
// the stale offset skip unshipped records.
func (s *Spool) compactSegment(id uint64, cursorOff int64) (int64, int64, time.Time, int64, error) {
	path := s.segmentPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, time.Time{}, 0, fmt.Errorf("read spool segment %d: %w", id, err)
	}
	var (
		kept    []byte
		records int64
		newest  time.Time
		rewrote bool
		mapped  = int64(-1)
	)
	for off := int64(0); off < int64(len(raw)); {
		rec, nextOff, ok := decodeRecordAt(raw, off)
		if ok {
			// A frame straddling the cursor is replayed; retries deduplicate.
			if cursorOff >= 0 && mapped < 0 && nextOff > cursorOff {
				mapped = int64(len(kept))
			}
			kept = append(kept, raw[off:nextOff]...)
			records++
			if rec.ObservedAt.After(newest) {
				newest = rec.ObservedAt
			}
			off = nextOff
			continue
		}
		s.recordCorruptLocked(raw, off)
		rewrote = true
		skip := skipLength(raw, off)
		if skip <= 0 {
			// Torn tail: the partial frame ends the segment.
			break
		}
		off += int64(skip)
	}
	if cursorOff >= 0 && mapped < 0 {
		mapped = int64(len(kept))
	}
	if rewrote {
		// Save the shorter offset first so a crash replays instead of skipping.
		if cursorOff >= 0 && mapped != cursorOff {
			s.cursor.Offset = mapped
			if err := s.persistCursorLocked(); err != nil {
				return 0, 0, time.Time{}, 0, err
			}
		}
		if err := s.replaceSegment(path, kept); err != nil {
			return 0, 0, time.Time{}, 0, fmt.Errorf("compact spool segment %d: %w", id, err)
		}
	}
	return int64(len(kept)), records, newest, mapped, nil
}

func (s *Spool) replaceSegment(path string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".segment-*.log")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if s.sync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// recordCorruptLocked counts one unreadable record as a loss under its
// best-effort recovered key (empty when the key bytes are gone).
func (s *Spool) recordCorruptLocked(raw []byte, off int64) {
	s.corrupt++
	s.droppedRecords++
	key := bestEffortRecordKey(raw, off)
	if s.corruptDrops[key] == 0 {
		s.corruptIDs[key] = SyntheticLineID()
	}
	s.corruptDrops[key]++
	if err := s.persistDropsLocked(); err != nil {
		slog.Warn("persist log spool drop counts", "error", err)
	}
}

// bestEffortRecordKey recovers the key from a damaged frame for attribution.
func bestEffortRecordKey(raw []byte, off int64) string {
	if off < 0 || off >= int64(len(raw)) {
		return ""
	}
	bodyLen, n := binary.Uvarint(raw[off:])
	if n <= 0 || bodyLen > 256<<20 {
		return ""
	}
	start := off + int64(n)
	end := start + int64(bodyLen)
	if end > int64(len(raw)) {
		end = int64(len(raw))
	}
	if start >= end {
		return ""
	}
	body := raw[start:end]
	keyLen, n := binary.Uvarint(body)
	if n <= 0 || keyLen > 1<<10 || uint64(len(body)-n) < keyLen {
		return ""
	}
	return string(body[n : n+int(keyLen)])
}

func (s *Spool) segmentPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf(spoolSegmentPattern, id))
}

// ErrSpoolFull rejects an append that cannot fit without evicting
// unshipped records (RejectOnFull spools).
var ErrSpoolFull = errors.New("log spool is full")

// ErrRecordTooLarge rejects a record that cannot fit the spool bounds.
var ErrRecordTooLarge = errors.New("log record exceeds the spool size cap")

// Append durably enqueues one payload, evicting oldest segments past the cap.
func (s *Spool) Append(key, id string, observedAt time.Time, payload []byte) error {
	encoded := encodeRecord(key, id, observedAt, payload)
	if int64(len(encoded)) > s.maxSeg {
		return ErrRecordTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	if s.active == nil {
		return errors.New("log spool has no writable segment")
	}
	if s.rejectOnFull {
		// Reclaim shipped capacity first; only unshippable overflow rejects.
		for s.bytes+int64(len(encoded)) > s.maxBytes {
			if !s.evictOneLocked() {
				break
			}
		}
		if s.bytes+int64(len(encoded)) > s.maxBytes {
			return ErrSpoolFull
		}
	}
	if s.activeSize+int64(len(encoded)) > s.maxSeg {
		if err := s.rotateLocked(); err != nil {
			return err
		}
	}
	if _, err := s.active.Write(encoded); err != nil {
		s.discardPartialAppendLocked()
		return fmt.Errorf("append to log spool: %w", err)
	}
	if s.sync {
		if err := s.active.Sync(); err != nil {
			s.discardPartialAppendLocked()
			return fmt.Errorf("sync log spool: %w", err)
		}
	}
	s.activeSize += int64(len(encoded))
	s.segments[len(s.segments)-1].size = s.activeSize
	s.segments[len(s.segments)-1].records++
	if observedAt.After(s.segments[len(s.segments)-1].newestObserved) {
		s.segments[len(s.segments)-1].newestObserved = observedAt
	}
	s.records++
	s.bytes += int64(len(encoded))
	if !s.rejectOnFull {
		s.evictLocked()
	}
	return nil
}

// discardPartialAppendLocked removes a failed append's bytes so a torn frame
// cannot hide later records. When the file cannot be truncated, the segment
// is sealed and rotated instead.
func (s *Spool) discardPartialAppendLocked() {
	if err := s.active.Truncate(s.activeSize); err == nil {
		return
	}
	_ = s.active.Close()
	s.active = nil
	_ = s.rotateLocked()
}

// Read returns up to maxRecords from the durable cursor. Pair every Read
// with Commit or Release.
func (s *Spool) Read(maxRecords int) ([]Record, Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, Cursor{}, errors.New("log spool is closed")
	}
	if maxRecords <= 0 {
		return nil, s.cursor, nil
	}
	var out []Record
	spanned := make(map[uint64]struct{})
	cursor := s.cursor
	for len(out) < maxRecords {
		raw, segID, err := s.readSegmentLocked(cursor.Segment)
		if err != nil {
			if errors.Is(err, errSegmentMissing) {
				// The cursor's segment was evicted; advance to the oldest survivor.
				next, ok := s.segmentAfterLocked(cursor.Segment)
				if !ok {
					cursor = Cursor{Segment: s.activeID, Offset: s.activeSize}
					break
				}
				cursor = Cursor{Segment: next, Offset: 0}
				continue
			}
			return nil, s.cursor, err
		}
		if cursor.Offset >= int64(len(raw)) {
			next, ok := s.segmentAfterLocked(cursor.Segment)
			if !ok {
				break
			}
			cursor = Cursor{Segment: next, Offset: 0}
			_ = segID
			continue
		}
		rec, nextOff, ok := decodeRecordAt(raw, cursor.Offset)
		if !ok {
			skip := skipLength(raw, cursor.Offset)
			if skip <= 0 {
				next, hasNext := s.segmentAfterLocked(cursor.Segment)
				if !hasNext {
					break
				}
				cursor = Cursor{Segment: next, Offset: 0}
				continue
			}
			s.recordCorruptLocked(raw, cursor.Offset)
			cursor.Offset += int64(skip)
			continue
		}
		out = append(out, rec)
		spanned[cursor.Segment] = struct{}{}
		cursor.Offset = nextOff
	}
	s.releaseInflightLocked()
	for id := range spanned {
		s.pinLocked(id)
	}
	return out, cursor, nil
}

func (s *Spool) pinLocked(id uint64) {
	for i := range s.segments {
		if s.segments[i].id == id {
			s.segments[i].pins++
			s.inflight[id] = struct{}{}
			return
		}
	}
}

func (s *Spool) releaseInflightLocked() {
	for id := range s.inflight {
		for i := range s.segments {
			if s.segments[i].id == id {
				s.segments[i].pins--
				break
			}
		}
	}
	s.inflight = make(map[uint64]struct{})
}

// Commit advances the durable cursor past a shipped batch. The cursor moves
// only once durably saved: on save failure the batch replays instead of
// being skipped.
func (s *Spool) Commit(c Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	defer s.releaseInflightLocked()
	if c.Segment < s.cursor.Segment ||
		(c.Segment == s.cursor.Segment && c.Offset < s.cursor.Offset) {
		return fmt.Errorf("log spool commit moved backwards: %+v after %+v", c, s.cursor)
	}
	previous := s.cursor
	s.cursor = c
	if err := s.persistCursorLocked(); err != nil {
		s.cursor = previous
		return err
	}
	s.collectLocked()
	return nil
}

// Release returns the outstanding batch unshipped for the next Read.
func (s *Spool) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseInflightLocked()
}

// RewindForReplay moves the cursor back to the first record at or after t.
// It never moves forward: unaccepted records always replay.
func (s *Spool) RewindForReplay(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	target := t.UnixNano()
	for _, seg := range s.segments {
		if seg.id > s.cursor.Segment {
			return nil
		}
		raw, err := os.ReadFile(s.segmentPath(seg.id))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read spool segment %d: %w", seg.id, err)
		}
		var off int64
		for off < int64(len(raw)) {
			rec, nextOff, ok := decodeRecordAt(raw, off)
			if !ok {
				skip := skipLength(raw, off)
				if skip <= 0 {
					break
				}
				off += int64(skip)
				continue
			}
			if rec.ObservedAt.UnixNano() >= target {
				pos := Cursor{Segment: seg.id, Offset: off}
				if pos.Segment < s.cursor.Segment ||
					(pos.Segment == s.cursor.Segment && pos.Offset <= s.cursor.Offset) {
					s.cursor = pos
					return s.persistCursorLocked()
				}
				// The window starts past unshipped records; never skip them.
				return nil
			}
			off = nextOff
		}
	}
	return nil
}

func (s *Spool) Stats() SpoolStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SpoolStats{
		Segments:       len(s.segments),
		Records:        s.records,
		Bytes:          s.bytes,
		PendingRecords: s.pendingLocked(),
		DroppedRecords: s.droppedRecords,
		DroppedBytes:   s.droppedBytes,
		CorruptRecords: s.corrupt,
	}
}

// pendingLocked counts records at or after the commit cursor.
func (s *Spool) pendingLocked() int64 {
	var pending int64
	for _, seg := range s.segments {
		if seg.id < s.cursor.Segment {
			continue
		}
		raw, err := os.ReadFile(s.segmentPath(seg.id))
		if err != nil {
			continue
		}
		var off int64
		if seg.id == s.cursor.Segment {
			off = s.cursor.Offset
		}
		for off < int64(len(raw)) {
			skip := skipLength(raw, off)
			if skip <= 0 {
				break
			}
			pending++
			off += int64(skip)
		}
	}
	return pending
}

// DrainDrops returns per-key drop counts since the last drain and clears them.
func (s *Spool) DrainDrops() (evicted, corrupt map[string]uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.evicted) > 0 {
		evicted = s.evicted
		s.evicted = make(map[string]uint64)
	}
	if len(s.corruptDrops) > 0 {
		corrupt = s.corruptDrops
		s.corruptDrops = make(map[string]uint64)
		s.corruptIDs = make(map[string]string)
	}
	if evicted != nil || corrupt != nil {
		// The caller now owns these counts; clear the durable copy.
		if err := s.persistDropsLocked(); err != nil {
			slog.Warn("clear log spool drop counts", "error", err)
		}
	}
	return evicted, corrupt
}

// CorruptDrop is a pending loss with its stable gap identity.
type CorruptDrop struct {
	Count uint64
	ID    string
}

// PendingCorruptDrops snapshots corruption losses without clearing them.
func (s *Spool) PendingCorruptDrops() map[string]CorruptDrop {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]CorruptDrop, len(s.corruptDrops))
	for key, count := range s.corruptDrops {
		out[key] = CorruptDrop{Count: count, ID: s.corruptIDs[key]}
	}
	return out
}

// AcknowledgeCorruptDrops removes a written snapshot from loss accounting.
func (s *Spool) AcknowledgeCorruptDrops(snapshot map[string]CorruptDrop) error {
	if len(snapshot) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, drop := range snapshot {
		if s.corruptIDs[key] != drop.ID || s.corruptDrops[key] < drop.Count {
			return fmt.Errorf("stale corrupt drop acknowledgement for %q", key)
		}
	}
	for key, drop := range snapshot {
		s.corruptDrops[key] -= drop.Count
		if s.corruptDrops[key] == 0 {
			delete(s.corruptDrops, key)
			delete(s.corruptIDs, key)
		} else {
			// Counts recorded during the write start a new gap lineage.
			s.corruptIDs[key] = SyntheticLineID()
		}
	}
	if err := s.persistDropsLocked(); err != nil {
		for key, drop := range snapshot {
			s.corruptDrops[key] += drop.Count
			s.corruptIDs[key] = drop.ID
		}
		return err
	}
	return nil
}

func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.active == nil {
		return nil
	}
	err := s.active.Close()
	s.active = nil
	return err
}

func (s *Spool) rotateLocked() error {
	if s.active != nil {
		if s.sync {
			if err := s.active.Sync(); err != nil {
				return fmt.Errorf("sync sealed spool segment: %w", err)
			}
		}
		if err := s.active.Close(); err != nil {
			return fmt.Errorf("seal spool segment: %w", err)
		}
		s.active = nil
	}
	id := s.nextSeg
	s.nextSeg++
	f, err := os.OpenFile(s.segmentPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, spoolFileMode)
	if err != nil {
		return fmt.Errorf("create spool segment: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		_ = f.Close()
		return err
	}
	s.activeID = id
	s.active = f
	s.activeSize = 0
	s.segments = append(s.segments, segmentInfo{id: id})
	return nil
}

func (s *Spool) fullyShippedLocked(seg segmentInfo) bool {
	if seg.id < s.cursor.Segment {
		return true
	}
	return seg.id == s.cursor.Segment && seg.size <= s.cursor.Offset
}

type spoolDropsJSON struct {
	Evicted    map[string]uint64 `json:"evicted"`
	Corrupt    map[string]uint64 `json:"corrupt"`
	CorruptIDs map[string]string `json:"corrupt_ids"`
}

func (s *Spool) dropsPath() string {
	return filepath.Join(s.dir, "drops.json")
}

// persistDropsLocked records drop counts before their records are deleted.
func (s *Spool) persistDropsLocked() error {
	raw, err := json.Marshal(spoolDropsJSON{Evicted: s.evicted, Corrupt: s.corruptDrops, CorruptIDs: s.corruptIDs})
	if err != nil {
		return fmt.Errorf("encode spool drops: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".drops-*.json")
	if err != nil {
		return fmt.Errorf("write spool drops: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write spool drops: %w", err)
	}
	if s.sync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("sync spool drops: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write spool drops: %w", err)
	}
	if err := os.Rename(tmpName, s.dropsPath()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit spool drops: %w", err)
	}
	return syncDir(s.dir)
}

func (s *Spool) loadDrops() {
	raw, err := os.ReadFile(s.dropsPath())
	if err != nil {
		return
	}
	var drops spoolDropsJSON
	if err := json.Unmarshal(raw, &drops); err != nil {
		return
	}
	for key, count := range drops.Evicted {
		s.evicted[key] += count
	}
	assignedID := false
	for key, count := range drops.Corrupt {
		s.corruptDrops[key] += count
		s.corruptIDs[key] = drops.CorruptIDs[key]
		if s.corruptIDs[key] == "" {
			s.corruptIDs[key] = SyntheticLineID()
			assignedID = true
		}
	}
	if assignedID {
		if err := s.persistDropsLocked(); err != nil {
			slog.Warn("persist recovered corrupt drop identities", "error", err)
		}
	}
}

// evictOneLocked evicts the oldest evictable segment, counting unshipped
// records as drops. It reports false when nothing can be evicted.
func (s *Spool) evictOneLocked() bool {
	idx := -1
	for i, seg := range s.segments {
		if seg.pins != 0 {
			continue
		}
		if s.rejectOnFull && !s.fullyShippedLocked(seg) {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		return false
	}
	oldest := s.segments[idx]
	if oldest.id == s.activeID {
		// Single-segment overflow: seal it, then evict the sealed data.
		if err := s.rotateLocked(); err != nil {
			return false
		}
		oldest = s.segments[idx]
	}
	drops, dropBytes := s.unshippedCountsLocked(oldest)
	for key, count := range drops {
		s.evicted[key] += count
		s.droppedRecords += count
	}
	s.droppedBytes += dropBytes
	if len(drops) > 0 {
		if err := s.persistDropsLocked(); err != nil {
			slog.Warn("persist log spool drop counts", "error", err)
			for key, count := range drops {
				s.evicted[key] -= count
				if s.evicted[key] == 0 {
					delete(s.evicted, key)
				}
				s.droppedRecords -= count
			}
			s.droppedBytes -= dropBytes
			return false
		}
	}
	s.removeSegmentLocked(oldest)
	return true
}

// evictLocked deletes oldest unpinned segments while past the byte cap.
// Pinned segments are never evicted; the overshoot is bounded by one batch.
func (s *Spool) evictLocked() {
	for s.bytes > s.maxBytes {
		if !s.evictOneLocked() {
			return
		}
	}
}

// unshippedCountsLocked counts records in seg at or after the commit cursor.
func (s *Spool) unshippedCountsLocked(seg segmentInfo) (map[string]uint64, uint64) {
	out := make(map[string]uint64)
	if seg.id < s.cursor.Segment {
		return out, 0
	}
	raw, err := os.ReadFile(s.segmentPath(seg.id))
	if err != nil {
		return out, 0
	}
	var (
		off   int64
		bytes uint64
	)
	for off < int64(len(raw)) {
		rec, nextOff, ok := decodeRecordAt(raw, off)
		if !ok {
			skip := skipLength(raw, off)
			if skip <= 0 {
				break
			}
			off += int64(skip)
			continue
		}
		if seg.id > s.cursor.Segment || off >= s.cursor.Offset {
			out[rec.Key]++
			bytes += uint64(nextOff - off)
		}
		off = nextOff
	}
	return out, bytes
}

func (s *Spool) removeSegmentLocked(seg segmentInfo) {
	path := s.segmentPath(seg.id)
	_ = os.Remove(path)
	_ = syncDir(s.dir)
	for i := range s.segments {
		if s.segments[i].id == seg.id {
			s.segments = append(s.segments[:i], s.segments[i+1:]...)
			break
		}
	}
	s.records -= seg.records
	s.bytes -= seg.size
	if s.cursor.Segment == seg.id {
		s.cursor.Offset = 0
		if len(s.segments) > 0 {
			s.cursor.Segment = s.segments[0].id
		} else {
			s.cursor.Segment = s.activeID
		}
	}
}

// collectLocked deletes sealed segments fully behind the commit cursor
// once past the retention horizon.
func (s *Spool) collectLocked() {
	cutoff := time.Now().Add(-s.retention)
	for len(s.segments) > 0 {
		oldest := s.segments[0]
		if oldest.id == s.activeID {
			return
		}
		if oldest.id > s.cursor.Segment {
			return
		}
		if oldest.id == s.cursor.Segment && s.cursor.Offset < oldest.size {
			return
		}
		if s.retention > 0 && !oldest.newestObserved.Before(cutoff) {
			return
		}
		s.removeSegmentLocked(oldest)
	}
}

var errSegmentMissing = errors.New("spool segment is missing")

func (s *Spool) readSegmentLocked(id uint64) ([]byte, uint64, error) {
	for _, seg := range s.segments {
		if seg.id == id {
			raw, err := os.ReadFile(s.segmentPath(id))
			if err != nil {
				if os.IsNotExist(err) {
					return nil, 0, errSegmentMissing
				}
				return nil, 0, fmt.Errorf("read spool segment %d: %w", id, err)
			}
			return raw, id, nil
		}
	}
	return nil, 0, errSegmentMissing
}

func (s *Spool) segmentAfterLocked(id uint64) (uint64, bool) {
	for _, seg := range s.segments {
		if seg.id > id {
			return seg.id, true
		}
	}
	return 0, false
}

type spoolCursorJSON struct {
	Segment uint64 `json:"segment"`
	Offset  int64  `json:"offset"`
}

func (s *Spool) cursorPath() string {
	return filepath.Join(s.dir, spoolCursorFile)
}

func (s *Spool) loadCursor() (Cursor, error) {
	raw, err := os.ReadFile(s.cursorPath())
	if err != nil {
		if os.IsNotExist(err) {
			return Cursor{}, nil
		}
		return Cursor{}, fmt.Errorf("read spool cursor: %w", err)
	}
	var decoded spoolCursorJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Cursor{}, fmt.Errorf("decode spool cursor: %w", err)
	}
	return Cursor{Segment: decoded.Segment, Offset: decoded.Offset}, nil
}

func (s *Spool) persistCursorLocked() error {
	raw, err := json.Marshal(spoolCursorJSON{Segment: s.cursor.Segment, Offset: s.cursor.Offset})
	if err != nil {
		return fmt.Errorf("encode spool cursor: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".cursor-*.json")
	if err != nil {
		return fmt.Errorf("write spool cursor: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write spool cursor: %w", err)
	}
	if s.sync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("sync spool cursor: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write spool cursor: %w", err)
	}
	if err := os.Rename(tmpName, s.cursorPath()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit spool cursor: %w", err)
	}
	return syncDir(s.dir)
}

// clampCursor pins a persisted cursor into the recovered segment range.
func (s *Spool) clampCursor(c Cursor) Cursor {
	if len(s.segments) == 0 {
		return Cursor{}
	}
	if c.Segment < s.segments[0].id {
		return Cursor{Segment: s.segments[0].id}
	}
	for _, seg := range s.segments {
		if seg.id == c.Segment {
			if c.Offset < 0 || c.Offset > seg.size {
				return Cursor{Segment: seg.id, Offset: seg.size}
			}
			return c
		}
	}
	last := s.segments[len(s.segments)-1]
	if c.Segment > last.id {
		return Cursor{Segment: last.id, Offset: last.size}
	}
	for _, seg := range s.segments {
		if seg.id > c.Segment {
			return Cursor{Segment: seg.id}
		}
	}
	return Cursor{Segment: last.id, Offset: last.size}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
}

var spoolCRC = crc32.MakeTable(crc32.Castagnoli)

// encodeRecord frames one record as uvarint(body length) + body + CRC32C(body).
func encodeRecord(key, id string, observedAt time.Time, payload []byte) []byte {
	body := make([]byte, 0, len(key)+len(id)+len(payload)+32)
	body = binary.AppendUvarint(body, uint64(len(key)))
	body = append(body, key...)
	body = binary.AppendUvarint(body, uint64(len(id)))
	body = append(body, id...)
	body = binary.AppendVarint(body, observedAt.UTC().UnixNano())
	body = binary.AppendUvarint(body, uint64(len(payload)))
	body = append(body, payload...)
	out := make([]byte, 0, len(body)+10)
	out = binary.AppendUvarint(out, uint64(len(body)))
	out = append(out, body...)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc32.Checksum(body, spoolCRC))
	out = append(out, sum[:]...)
	return out
}

// decodeRecordAt decodes the record at off; ok=false means corrupt or torn.
func decodeRecordAt(raw []byte, off int64) (Record, int64, bool) {
	if off < 0 || off >= int64(len(raw)) {
		return Record{}, off, false
	}
	bodyLen, n := binary.Uvarint(raw[off:])
	if n <= 0 {
		return Record{}, off, false
	}
	start := off + int64(n)
	end := start + int64(bodyLen) + 4
	if bodyLen > 256<<20 || end > int64(len(raw)) {
		return Record{}, off, false
	}
	body := raw[start : start+int64(bodyLen)]
	if binary.BigEndian.Uint32(raw[start+int64(bodyLen):end]) != crc32.Checksum(body, spoolCRC) {
		return Record{}, off, false
	}
	rec, ok := decodeRecordBody(body)
	if !ok {
		return Record{}, off, false
	}
	return rec, end, true
}

func decodeRecordBody(body []byte) (Record, bool) {
	var rec Record
	keyLen, n := binary.Uvarint(body)
	if n <= 0 || uint64(len(body[n:])) < keyLen {
		return Record{}, false
	}
	body = body[n:]
	rec.Key = string(body[:keyLen])
	body = body[keyLen:]
	idLen, n := binary.Uvarint(body)
	if n <= 0 || uint64(len(body[n:])) < idLen {
		return Record{}, false
	}
	body = body[n:]
	rec.ID = string(body[:idLen])
	body = body[idLen:]
	nanos, n := binary.Varint(body)
	if n <= 0 {
		return Record{}, false
	}
	body = body[n:]
	rec.ObservedAt = time.Unix(0, nanos).UTC()
	payloadLen, n := binary.Uvarint(body)
	if n <= 0 || uint64(len(body[n:])) != payloadLen {
		return Record{}, false
	}
	rec.Payload = append([]byte(nil), body[n:]...)
	return rec, true
}

// skipLength returns the framed record length at off, or -1 when unreadable.
func skipLength(raw []byte, off int64) int {
	if off < 0 || off >= int64(len(raw)) {
		return -1
	}
	bodyLen, n := binary.Uvarint(raw[off:])
	if n <= 0 || bodyLen > 256<<20 {
		return -1
	}
	total := int64(n) + int64(bodyLen) + 4
	if off+total > int64(len(raw)) {
		return -1
	}
	return int(total)
}
