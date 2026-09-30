package logpipeline

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

const (
	defaultSpoolMaxBytes   = 256 << 20
	defaultSpoolMaxRecords = 1_000_000
	SpoolFile              = "logs.db"
	// Page layout, copy-on-write transactions and durable drop metadata need
	// space beyond the retained record budget. MaxSize enforces this disk cap.
	spoolDiskReserve = 8 << 20
)

var (
	ErrSpoolFull      = errors.New("log spool is full")
	ErrRecordTooLarge = errors.New("log record exceeds the spool size cap")
	recordsBucket     = []byte("records")
	stateBucket       = []byte("state")
	stateKey          = []byte("queue")
)

// Record contains an opaque payload and the attribution needed if it is shed.
// Stable line identities belong to the payload and survive every retry.
type Record struct {
	DropKey    DropKey
	ObservedAt time.Time
	Payload    []byte
	sequence   uint64
	readCursor Cursor
}

// Cursor is an opaque acknowledgement token returned by Read. One consumer
// pairs every Read with Commit or Release; stale tokens cannot skip records.
type Cursor struct {
	after, generation uint64
	owner             *Spool
}

// SpoolStats separates retained encoded bytes from the database's disk budget.
type SpoolStats struct {
	Records        int64
	Bytes          int64
	PendingRecords int64
	DroppedRecords uint64
	DroppedBytes   uint64
	DiskBytes      int64
	DiskLimit      int64
}

// SpoolConfig bounds one durable FIFO. MaxBytes includes encoded record
// metadata. Committed records are retained for replay until Retention expires,
// but may be collected sooner under pressure without counting as drops.
type SpoolConfig struct {
	Dir          string
	MaxBytes     int64
	MaxRecords   int64
	RejectOnFull bool
	Retention    time.Duration
}

// Spool persists records, the acknowledgement cursor and drop summaries in
// bbolt transactions. No read transaction stays open during network delivery.
type Spool struct {
	mu                              sync.Mutex
	db                              *bolt.DB
	maxBytes, maxRecords, diskLimit int64
	rejectOnFull                    bool
	retention                       time.Duration
	inflight                        *Cursor
	generation                      uint64
}

type queueState struct {
	After                        uint64
	Records, Bytes, Pending      int64
	DroppedRecords, DroppedBytes uint64
}

func OpenSpool(cfg SpoolConfig) (*Spool, error) {
	if cfg.Dir == "" {
		return nil, errors.New("log spool directory is required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultSpoolMaxBytes
	}
	if cfg.MaxRecords <= 0 {
		cfg.MaxRecords = defaultSpoolMaxRecords
	}
	if cfg.MaxBytes > (int64(^uint(0)>>1)-spoolDiskReserve)/4 {
		return nil, errors.New("log spool byte cap exceeds supported database size")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log spool: %w", err)
	}
	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "seg-") || entry.Name() == "cursor.json" || entry.Name() == "drops.json" || entry.Name() == "pending-drops.json" {
			return nil, fmt.Errorf("log spool %s uses the retired segmented format; drain it with the previous release or explicitly reset the directory before cutover", cfg.Dir)
		}
	}
	diskLimit := 4*cfg.MaxBytes + spoolDiskReserve
	db, err := bolt.Open(filepath.Join(cfg.Dir, SpoolFile), 0o600, &bolt.Options{Timeout: time.Second, MaxSize: int(diskLimit)})
	if err != nil {
		return nil, fmt.Errorf("open log spool database: %w", err)
	}
	db.AllocSize = 1 << 20
	s := &Spool{db: db, maxBytes: cfg.MaxBytes, maxRecords: cfg.MaxRecords, diskLimit: diskLimit, rejectOnFull: cfg.RejectOnFull, retention: cfg.Retention}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{recordsBucket, stateBucket, dropsBucket, dropKeysBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if tx.Bucket(stateBucket).Get(stateKey) == nil {
			return saveState(tx, queueState{})
		}
		_, err := loadState(tx)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := syncSpoolDirectory(filepath.Dir(cfg.Dir)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := syncSpoolDirectory(cfg.Dir); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func loadState(tx *bolt.Tx) (queueState, error) {
	var state queueState
	err := json.Unmarshal(tx.Bucket(stateBucket).Get(stateKey), &state)
	return state, err
}
func saveState(tx *bolt.Tx, state queueState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return tx.Bucket(stateBucket).Put(stateKey, raw)
}
func sequenceKey(n uint64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], n)
	return key[:]
}
func decodeRecord(raw []byte) (Record, error) {
	var record Record
	err := json.Unmarshal(raw, &record)
	return record, err
}
func spoolError(err error) error {
	if errors.Is(err, berrors.ErrMaxSizeReached) {
		return fmt.Errorf("%w: database disk budget reached", ErrSpoolFull)
	}
	return err
}

// Append durably admits a complete batch. Eviction and attributable drop
// summaries commit together; failure changes neither records nor accounting.
func (s *Spool) Append(records ...Record) error {
	if len(records) == 0 {
		return nil
	}
	encoded := make([][]byte, len(records))
	var bytes int64
	for i, record := range records {
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		encoded[i] = raw
		bytes += int64(len(raw) + 8)
	}
	if bytes > s.maxBytes || int64(len(records)) > s.maxRecords {
		return ErrRecordTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return spoolError(s.db.Update(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		if err := s.collect(tx, &state, false); err != nil {
			return err
		}
		for state.Bytes+bytes > s.maxBytes || state.Records+int64(len(records)) > s.maxRecords {
			if err := s.evict(tx, &state); err != nil {
				return err
			}
		}
		bucket := tx.Bucket(recordsBucket)
		for _, raw := range encoded {
			seq, err := bucket.NextSequence()
			if err != nil {
				return err
			}
			if err := bucket.Put(sequenceKey(seq), raw); err != nil {
				return err
			}
			state.Records++
			state.Pending++
			state.Bytes += int64(len(raw) + 8)
		}
		return saveState(tx, state)
	}))
}

func (s *Spool) remove(tx *bolt.Tx, state *queueState, key, raw []byte, reason string) error {
	seq := binary.BigEndian.Uint64(key)
	if reason != "" {
		record, err := decodeRecord(raw)
		if err != nil {
			return fmt.Errorf("decode log spool record: %w", err)
		}
		if record.DropKey == (DropKey{}) {
			return errors.New("cannot evict an unattributable log record")
		}
		key := record.DropKey
		key.Reason = reason
		if err := addDrop(tx, Drop{Key: key, Count: 1, Start: record.ObservedAt, End: record.ObservedAt}); err != nil {
			return err
		}
		state.DroppedRecords++
		state.DroppedBytes += uint64(len(raw) + 8)
	}
	if err := tx.Bucket(recordsBucket).Delete(key); err != nil {
		return err
	}
	state.Records--
	state.Bytes -= int64(len(raw) + 8)
	if seq > state.After {
		state.Pending--
	}
	return nil
}

// collect reclaims acknowledged records; pressure ignores the replay horizon.
func (s *Spool) collect(tx *bolt.Tx, state *queueState, pressure bool) error {
	c := tx.Bucket(recordsBucket).Cursor()
	cutoff := time.Now().Add(-s.retention)
	for key, raw := c.First(); key != nil; {
		if binary.BigEndian.Uint64(key) > state.After {
			break
		}
		if !pressure && s.retention > 0 {
			record, err := decodeRecord(raw)
			if err != nil {
				return err
			}
			if !record.ObservedAt.Before(cutoff) {
				key, raw = c.Next()
				continue
			}
		}
		deleted := append([]byte(nil), key...)
		if err := s.remove(tx, state, key, raw, ""); err != nil {
			return err
		}
		// Delete can shift a materialized bbolt node under this cursor.
		// Seeking the deleted key lands on its next survivor without skipping.
		key, raw = c.Seek(deleted)
	}
	return nil
}

func (s *Spool) evict(tx *bolt.Tx, state *queueState) error {
	// Acknowledged replay copies yield to unacknowledged output first.
	before := state.Records
	if err := s.collect(tx, state, true); err != nil {
		return err
	}
	if state.Records < before {
		return nil
	}
	if s.rejectOnFull {
		return ErrSpoolFull
	}
	c := tx.Bucket(recordsBucket).Cursor()
	for key, raw := c.First(); key != nil; key, raw = c.Next() {
		seq := binary.BigEndian.Uint64(key)
		if s.inflight != nil && seq > state.After && seq <= s.inflight.after {
			continue
		}
		return s.remove(tx, state, key, raw, ReasonSpoolOverflow)
	}
	return ErrSpoolFull
}

// Read copies a bounded batch and pins those records until Commit or Release.
func (s *Spool) Read(maxRecords int) ([]Record, Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != nil {
		return nil, Cursor{}, errors.New("log spool already has an outstanding read")
	}
	var records []Record
	s.generation++
	cursor := Cursor{generation: s.generation, owner: s}
	err := s.db.View(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		cursor.after = state.After
		c := tx.Bucket(recordsBucket).Cursor()
		for key, raw := c.Seek(sequenceKey(state.After + 1)); key != nil && len(records) < maxRecords; key, raw = c.Next() {
			record, err := decodeRecord(raw)
			if err != nil {
				return fmt.Errorf("decode log spool record: %w", err)
			}
			record.sequence = binary.BigEndian.Uint64(key)
			records = append(records, record)
			cursor.after = record.sequence
		}
		return nil
	})
	if err == nil {
		for i := range records {
			records[i].readCursor = cursor
		}
		s.inflight = &cursor
	}
	return records, cursor, err
}

// Commit atomically acknowledges both a read batch and its delivered drop
// snapshot. On any storage failure both replay unchanged.
func (s *Spool) Commit(cursor Cursor, drops []*platformv1.LogDropSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil || *s.inflight != cursor {
		return errors.New("stale log spool acknowledgement")
	}
	defer func() { s.inflight = nil }()
	return spoolError(s.db.Update(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		c := tx.Bucket(recordsBucket).Cursor()
		for key, _ := c.Seek(sequenceKey(state.After + 1)); key != nil && binary.BigEndian.Uint64(key) <= cursor.after; key, _ = c.Next() {
			state.Pending--
		}
		state.After = cursor.after
		if err := acknowledgeDrops(tx, drops); err != nil {
			return err
		}
		if err := s.collect(tx, &state, false); err != nil {
			return err
		}
		return saveState(tx, state)
	}))
}

func (s *Spool) Release() { s.mu.Lock(); defer s.mu.Unlock(); s.inflight = nil }

// Discard converts an undecodable producer payload from the outstanding read
// into an attributed corruption gap atomically. Other records remain deliverable.
func (s *Spool) Discard(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil || *s.inflight != record.readCursor {
		return errors.New("stale log record discard")
	}
	return spoolError(s.db.Update(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		key := sequenceKey(record.sequence)
		raw := tx.Bucket(recordsBucket).Get(key)
		if raw == nil {
			return errors.New("log record was already discarded")
		}
		if err := s.remove(tx, &state, key, raw, ReasonCorruptSpool); err != nil {
			return err
		}
		return saveState(tx, state)
	}))
}

// RewindForReplay never skips unacknowledged records. Rewinding while a batch
// is in flight is refused so its acknowledgement cannot erase the replay.
func (s *Spool) RewindForReplay(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != nil {
		return errors.New("cannot rewind an outstanding log batch")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		c := tx.Bucket(recordsBucket).Cursor()
		for key, raw := c.First(); key != nil; key, raw = c.Next() {
			seq := binary.BigEndian.Uint64(key)
			if seq > state.After {
				break
			}
			record, err := decodeRecord(raw)
			if err != nil {
				return err
			}
			if !record.ObservedAt.Before(t) {
				for later, _ := c.Seek(sequenceKey(seq)); later != nil && binary.BigEndian.Uint64(later) <= state.After; later, _ = c.Next() {
					state.Pending++
				}
				state.After = seq - 1
				return saveState(tx, state)
			}
		}
		return nil
	})
}

// Abandon turns a dead build attempt's unaccepted output into durable,
// attributed gaps. A retried attempt inherits these summaries, never its lines.
func (s *Spool) Abandon() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != nil {
		return errors.New("cannot abandon an outstanding log batch")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		c := tx.Bucket(recordsBucket).Cursor()
		for key, raw := c.First(); key != nil; key, raw = c.First() {
			reason := ""
			if binary.BigEndian.Uint64(key) > state.After {
				reason = ReasonSpoolOverflow
			}
			if err := s.remove(tx, &state, key, raw, reason); err != nil {
				return err
			}
		}
		return saveState(tx, state)
	})
}

func (s *Spool) Stats() (SpoolStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := SpoolStats{DiskLimit: s.diskLimit}
	err := s.db.View(func(tx *bolt.Tx) error {
		state, err := loadState(tx)
		if err != nil {
			return err
		}
		stats.Records = state.Records
		stats.Bytes = state.Bytes
		stats.PendingRecords = state.Pending
		stats.DroppedRecords = state.DroppedRecords
		stats.DroppedBytes = state.DroppedBytes
		return nil
	})
	if info, err := os.Stat(s.db.Path()); err == nil {
		stats.DiskBytes = info.Size()
	}
	return stats, err
}
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = nil
	return s.db.Close()
}

func syncSpoolDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
