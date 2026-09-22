package logpipeline

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
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

// Record is one spooled payload. Key attributes the record for drop
// accounting (allocation ID on agents, build ID on builders); ID is
// the stable line identity used for server-side deduplication.
type Record struct {
	Key        string
	ID         string
	ObservedAt time.Time
	Payload    []byte
}

// Cursor is an opaque read position: the next unread byte offset in
// the named segment.
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
	// MaxBytes caps total spool size. Oldest segments are evicted
	// past the cap; unshipped evicted records count as drops.
	// Defaults to 256 MiB.
	MaxBytes int64
	// MaxSegmentBytes rotates the active segment past this size.
	// Defaults to 8 MiB.
	MaxSegmentBytes int64
	// SyncWrites fsyncs every append. Disable only in tests.
	SyncWrites bool
}

// Spool is a bounded disk-backed FIFO queue. Appends are durable
// (with SyncWrites) and survive crashes; reads replay from the last
// commit; commits advance the durable cursor and garbage-collect
// fully shipped segments. When the byte cap is exceeded the oldest
// segments are evicted and their unshipped records counted per key
// so callers can report explicit read gaps.
type Spool struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	maxSeg   int64
	sync     bool

	activeID   uint64
	active     *os.File
	activeSize int64
	nextSeg    uint64

	segments []segmentInfo
	cursor   Cursor

	records        int64
	bytes          int64
	dropped        map[string]uint64
	droppedRecords uint64
	droppedBytes   uint64
	corrupt        uint64
	closed         bool
}

type segmentInfo struct {
	id      uint64
	size    int64
	records int64
}

// OpenSpool opens or creates the spool in cfg.Dir, recovering segment
// files and the durable cursor. Torn tail writes from a crash are
// truncated; mid-file corrupt records are compacted out and counted.
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
	if err := os.MkdirAll(cfg.Dir, spoolDirMode); err != nil {
		return nil, fmt.Errorf("create log spool dir: %w", err)
	}
	s := &Spool{
		dir:      cfg.Dir,
		maxBytes: maxBytes,
		maxSeg:   maxSeg,
		sync:     cfg.SyncWrites,
		dropped:  make(map[string]uint64),
	}
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
	for _, id := range ids {
		size, records, err := s.compactSegment(id)
		if err != nil {
			return err
		}
		s.segments = append(s.segments, segmentInfo{id: id, size: size, records: records})
		s.records += records
		s.bytes += size
		if id >= s.nextSeg {
			s.nextSeg = id + 1
		}
	}
	cursor, err := s.loadCursor()
	if err != nil {
		return err
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

// compactSegment drops corrupt records from one segment, truncates a
// torn tail, and returns the resulting size and record count.
func (s *Spool) compactSegment(id uint64) (int64, int64, error) {
	path := s.segmentPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read spool segment %d: %w", id, err)
	}
	records, validThrough, corrupt := scanRecords(raw)
	if corrupt == 0 && int64(len(raw)) == validThrough {
		return int64(len(raw)), records, nil
	}
	// Rewrite the valid prefix, skipping corrupt mid-file records.
	kept := rewriteValidPrefix(raw, validThrough)
	if err := os.WriteFile(path, kept, spoolFileMode); err != nil {
		return 0, 0, fmt.Errorf("compact spool segment %d: %w", id, err)
	}
	s.corrupt += corrupt
	// A torn tail loses at most one partial record; count it once.
	if int64(len(raw)) > validThrough {
		s.corrupt++
		s.dropped[""]++
		s.droppedRecords++
	}
	return int64(len(kept)), records, nil
}

func (s *Spool) segmentPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf(spoolSegmentPattern, id))
}

// Append durably enqueues one payload, evicting the oldest segments
// past the byte cap. Evicted unshipped records are counted per key.
func (s *Spool) Append(key, id string, observedAt time.Time, payload []byte) error {
	encoded := encodeRecord(key, id, observedAt, payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	if s.activeSize+int64(len(encoded)) > s.maxSeg {
		if err := s.rotateLocked(); err != nil {
			return err
		}
	}
	if _, err := s.active.Write(encoded); err != nil {
		return fmt.Errorf("append to log spool: %w", err)
	}
	if s.sync {
		if err := s.active.Sync(); err != nil {
			return fmt.Errorf("sync log spool: %w", err)
		}
	}
	s.activeSize += int64(len(encoded))
	s.segments[len(s.segments)-1].size = s.activeSize
	s.segments[len(s.segments)-1].records++
	s.records++
	s.bytes += int64(len(encoded))
	s.evictLocked()
	return nil
}

// Read returns up to maxRecords starting at the durable cursor. The
// returned cursor commits the batch once the caller ships it.
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
	cursor := s.cursor
	for len(out) < maxRecords {
		raw, segID, err := s.readSegmentLocked(cursor.Segment)
		if err != nil {
			if errors.Is(err, errSegmentMissing) {
				// The cursor's segment was evicted; advance to the
				// oldest surviving segment.
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
			// Mid-file corruption after open: skip the record and
			// count it once under the unknown key.
			skip := skipLength(raw, cursor.Offset)
			if skip <= 0 {
				next, hasNext := s.segmentAfterLocked(cursor.Segment)
				if !hasNext {
					break
				}
				cursor = Cursor{Segment: next, Offset: 0}
				continue
			}
			cursor.Offset += int64(skip)
			s.corrupt++
			s.dropped[""]++
			s.droppedRecords++
			continue
		}
		out = append(out, rec)
		cursor.Offset = nextOff
	}
	return out, cursor, nil
}

// Commit advances the durable cursor past a shipped batch and deletes
// fully shipped sealed segments.
func (s *Spool) Commit(c Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	if c.Segment < s.cursor.Segment ||
		(c.Segment == s.cursor.Segment && c.Offset < s.cursor.Offset) {
		return fmt.Errorf("log spool commit moved backwards: %+v after %+v", c, s.cursor)
	}
	s.cursor = c
	if err := s.persistCursorLocked(); err != nil {
		return err
	}
	s.collectLocked()
	return nil
}

// RewindToTime moves the durable cursor to the first record with
// timestamp at or after t, so a reconnect replays the recent window
// and retried lines deduplicate server-side by line ID.
func (s *Spool) RewindToTime(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("log spool is closed")
	}
	target := t.UnixNano()
	for _, seg := range s.segments {
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
				s.cursor = Cursor{Segment: seg.id, Offset: off}
				return s.persistCursorLocked()
			}
			off = nextOff
		}
	}
	return nil
}

// Stats reports spool health and lifetime counters.
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

// pendingLocked counts records at or after the commit cursor by
// walking segment frames without decoding payloads.
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

// DrainDrops returns per-key evicted counts since the last drain and
// clears them. The empty key holds corrupt or unattributable drops.
func (s *Spool) DrainDrops() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dropped) == 0 {
		return nil
	}
	out := s.dropped
	s.dropped = make(map[string]uint64)
	return out
}

// Close flushes and closes the spool.
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

// evictLocked deletes the oldest segments while the spool exceeds
// its byte cap. Fully shipped segments vanish silently; unshipped
// records are counted per key as drops.
func (s *Spool) evictLocked() {
	for s.bytes > s.maxBytes && len(s.segments) > 1 {
		oldest := s.segments[0]
		if oldest.id == s.activeID {
			// Single-segment overflow: seal it so the writer keeps
			// a live active segment, then evict the sealed data.
			if err := s.rotateLocked(); err != nil {
				return
			}
			oldest = s.segments[0]
		}
		drops, dropBytes := s.unshippedCountsLocked(oldest)
		for key, count := range drops {
			s.dropped[key] += count
			s.droppedRecords += count
		}
		s.droppedBytes += dropBytes
		s.removeSegmentLocked(oldest)
	}
}

// unshippedCountsLocked counts records in seg at or after the commit
// cursor. Segments fully behind the cursor were shipped already.
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
	s.segments = s.segments[1:]
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

// collectLocked deletes sealed segments fully behind the commit
// cursor. Committed data was shipped; no drop is counted.
func (s *Spool) collectLocked() {
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

// clampCursor pins a persisted cursor into the recovered segment
// range. A cursor past the end means everything shipped.
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

// encodeRecord frames one record as uvarint(body length) + body +
// CRC32C(body), where body carries key, ID, timestamp, and payload.
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

// decodeRecordAt decodes the record starting at off. ok=false means
// the bytes there are corrupt or a torn tail, never a valid record.
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

// skipLength returns the framed length of the record at off without
// verifying it, or -1 when the frame itself is unreadable.
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

// scanRecords walks raw counting valid records, the valid prefix
// length, and corrupt records. A torn tail stops the scan.
func scanRecords(raw []byte) (records int64, validThrough int64, corrupt uint64) {
	var off int64
	for off < int64(len(raw)) {
		_, nextOff, ok := decodeRecordAt(raw, off)
		if !ok {
			if skipLength(raw, off) > 0 {
				corrupt++
				off += int64(skipLength(raw, off))
				continue
			}
			break
		}
		records++
		off = nextOff
	}
	return records, off, corrupt
}

// rewriteValidPrefix copies the valid records below validThrough,
// skipping corrupt mid-file frames.
func rewriteValidPrefix(raw []byte, validThrough int64) []byte {
	var out []byte
	var off int64
	for off < validThrough {
		_, nextOff, ok := decodeRecordAt(raw, off)
		if !ok {
			skip := skipLength(raw, off)
			if skip <= 0 {
				break
			}
			off += int64(skip)
			continue
		}
		out = append(out, raw[off:nextOff]...)
		off = nextOff
	}
	return out
}
