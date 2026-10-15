package metering

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"github.com/google/uuid"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	journalFileName = "usage-journal.db"
	journalFormat   = 1
	// recordOverhead approximates the bbolt key, header, and page cost of
	// one record beyond its encoded bytes. TestJournalFileGrowth measures it.
	recordOverhead = 48
)

var (
	metaBucket    = []byte("meta")
	recordsBucket = []byte("records")
	statesBucket  = []byte("states")
	metaKey       = []byte("meta")

	errJournalFull = errors.New("usage journal is full")
)

// journalMeta is the producer identity and sequence state. It commits in
// the same transaction as the records and checkpoints it describes.
type journalMeta struct {
	Format         int    `json:"format"`
	ProducerID     string `json:"producer_id"`
	InstallationID string `json:"installation_id"`
	AgentID        string `json:"agent_id"`
	NextSequence   uint64 `json:"next_sequence"`
	AckedThrough   uint64 `json:"acked_through"`
	// LowestRetained is the lowest stored sequence. Lower sequences were
	// pruned after an acknowledgement and can no longer be resent.
	LowestRetained uint64   `json:"lowest_retained"`
	RecordBytes    int64    `json:"record_bytes"`
	Timeline       timeline `json:"timeline"`
	// LastTick is the last sample point folded into every resource.
	LastTick *point `json:"last_tick,omitempty"`
	// SealedThroughMS: all usage before this time is in stored records.
	SealedThroughMS  int64  `json:"sealed_through_ms"`
	ExhaustedSinceMS int64  `json:"exhausted_since_ms,omitempty"`
	GapRecords       uint64 `json:"gap_records"`
	// Unattributed is the cumulative kernel count of output bytes without
	// an epoch counter, as last read.
	Unattributed uint64 `json:"unattributed_egress_bytes"`
	LastError    string `json:"last_error,omitempty"`
}

type journal struct {
	db             *bbolt.DB
	maxBytes       int64
	ackedRetention time.Duration
	meta           journalMeta
}

// openJournal opens the usage journal, creating a new producer identity
// for a new file. A corrupt journal, or one from another installation or
// agent, is moved aside and replaced: its unacknowledged records cannot be
// trusted or delivered, and the control plane records the replacement as
// an explicit producer gap.
func openJournal(dir, installationID, agentID string, maxBytes int64, ackedRetention time.Duration) (*journal, error) {
	if installationID == "" || agentID == "" {
		return nil, errors.New("usage journal requires an installation and agent identity")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create usage journal directory: %w", err)
	}
	path := filepath.Join(dir, journalFileName)
	j, err := tryOpenJournal(path, installationID, agentID)
	if err != nil {
		aside := fmt.Sprintf("%s.replaced-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		slog.Error("usage journal unusable; starting a new producer", "path", path, "moved_to", aside, "error", err)
		if renameErr := os.Rename(path, aside); renameErr != nil && !errors.Is(renameErr, os.ErrNotExist) {
			return nil, errors.Join(err, renameErr)
		}
		if j, err = tryOpenJournal(path, installationID, agentID); err != nil {
			return nil, err
		}
	}
	j.maxBytes, j.ackedRetention = maxBytes, ackedRetention
	return j, nil
}

var errForeignJournal = errors.New("usage journal belongs to another installation or agent")

func tryOpenJournal(path, installationID, agentID string) (*journal, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if errors.Is(err, bbolt.ErrTimeout) {
		return nil, fmt.Errorf("open usage journal: locked by another agent process")
	}
	if err != nil {
		return nil, fmt.Errorf("open usage journal: %w", err)
	}
	j := &journal{db: db}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{metaBucket, recordsBucket, statesBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		raw := tx.Bucket(metaBucket).Get(metaKey)
		if raw == nil {
			j.meta = journalMeta{Format: journalFormat, ProducerID: uuid.NewString(), InstallationID: installationID, AgentID: agentID, NextSequence: 1, LowestRetained: 1}
			return putMeta(tx, &j.meta)
		}
		if err := json.Unmarshal(raw, &j.meta); err != nil {
			return fmt.Errorf("decode usage journal meta: %w", err)
		}
		if j.meta.Format != journalFormat || j.meta.NextSequence == 0 {
			return fmt.Errorf("unsupported usage journal format %d", j.meta.Format)
		}
		if j.meta.InstallationID != installationID || j.meta.AgentID != agentID {
			return errForeignJournal
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return j, nil
}

func putMeta(tx *bbolt.Tx, meta *journalMeta) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return tx.Bucket(metaBucket).Put(metaKey, raw)
}

func (j *journal) Close() error { return j.db.Close() }

func (j *journal) capacity() int64 { return j.maxBytes }

func sequenceKey(sequence uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, sequence)
}

// journalTx is one atomic journal update: checkpoints, records, and the
// sequence state commit together or not at all.
type journalTx struct {
	tx   *bbolt.Tx
	j    *journal
	meta journalMeta
}

// update runs fn in one bbolt transaction. On error nothing commits and the
// cached meta is unchanged.
func (j *journal) update(fn func(*journalTx) error) error {
	next := j.meta
	err := j.db.Update(func(tx *bbolt.Tx) error {
		jt := &journalTx{tx: tx, j: j, meta: next}
		if err := fn(jt); err != nil {
			return err
		}
		next = jt.meta
		return putMeta(tx, &jt.meta)
	})
	if err == nil {
		j.meta = next
	}
	return err
}

// append seals a record under the next sequence. It refuses to exceed the
// journal budget: unacknowledged records are never evicted to make room.
func (t *journalTx) append(record *agentv1.UsageRecord) error {
	record.InstallationId, record.ProducerId = t.meta.InstallationID, t.meta.ProducerID
	record.Sequence = t.meta.NextSequence
	record.MeterVersion = agentv1.UsageMeterVersion
	payload, err := proto.Marshal(record)
	if err != nil {
		return err
	}
	if t.meta.RecordBytes+int64(len(payload))+recordOverhead > t.j.capacity() {
		t.pruneAcknowledged(true)
		if t.meta.RecordBytes+int64(len(payload))+recordOverhead > t.j.capacity() {
			return errJournalFull
		}
	}
	if err := t.tx.Bucket(recordsBucket).Put(sequenceKey(record.Sequence), payload); err != nil {
		return err
	}
	t.meta.NextSequence++
	t.meta.RecordBytes += int64(len(payload)) + recordOverhead
	if record.GetKind() == agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP {
		t.meta.GapRecords++
	}
	return nil
}

func (t *journalTx) putState(key string, state *epochState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return t.tx.Bucket(statesBucket).Put([]byte(key), raw)
}

func (t *journalTx) deleteState(key string) error {
	return t.tx.Bucket(statesBucket).Delete([]byte(key))
}

// pruneAcknowledged deletes acknowledged records older than the recovery
// window. Under pressure it also deletes younger acknowledged records,
// oldest first; it never touches unacknowledged records.
func (t *journalTx) pruneAcknowledged(pressure bool) {
	bucket := t.tx.Bucket(recordsBucket)
	cutoff := time.Now().Add(-t.j.ackedRetention)
	cursor := bucket.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.First() {
		sequence := binary.BigEndian.Uint64(key)
		if sequence > t.meta.AckedThrough {
			break
		}
		if !pressure {
			var record agentv1.UsageRecord
			if proto.Unmarshal(value, &record) == nil && record.GetEnd().AsTime().After(cutoff) {
				break
			}
		}
		t.meta.RecordBytes -= int64(len(value)) + recordOverhead
		if err := cursor.Delete(); err != nil {
			return
		}
		t.meta.LowestRetained = sequence + 1
		if pressure && t.meta.RecordBytes < t.j.capacity()*9/10 {
			break
		}
	}
}

// states loads every persisted epoch checkpoint.
func (j *journal) states() (map[string]*epochState, error) {
	out := make(map[string]*epochState)
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(statesBucket).ForEach(func(key, value []byte) error {
			var state epochState
			if err := json.Unmarshal(value, &state); err != nil {
				return fmt.Errorf("decode usage checkpoint %s: %w", key, err)
			}
			out[string(key)] = &state
			return nil
		})
	})
	return out, err
}

// acknowledge applies the control plane's durable cursor. A cursor behind
// the local position means the control plane lost accepted records, for
// example after a database restore; retained records are resent from it.
func (j *journal) acknowledge(cursor uint64) error {
	if cursor >= j.meta.NextSequence {
		return fmt.Errorf("acknowledged sequence %d was never produced (next %d)", cursor, j.meta.NextSequence)
	}
	if cursor == j.meta.AckedThrough {
		return nil
	}
	return j.update(func(t *journalTx) error {
		t.meta.AckedThrough = cursor
		t.pruneAcknowledged(false)
		return nil
	})
}

// batch reads up to the report limits starting at from. lostThrough is set
// when from was already pruned.
func (j *journal) batch(from uint64) (records []*agentv1.UsageRecord, lostThrough uint64, err error) {
	err = j.db.View(func(tx *bbolt.Tx) error {
		var meta journalMeta
		if err := json.Unmarshal(tx.Bucket(metaBucket).Get(metaKey), &meta); err != nil {
			return err
		}
		if from < meta.LowestRetained {
			lostThrough = meta.LowestRetained - 1
			from = meta.LowestRetained
		}
		size := 0
		cursor := tx.Bucket(recordsBucket).Cursor()
		for key, value := cursor.Seek(sequenceKey(from)); key != nil; key, value = cursor.Next() {
			if len(records) == agentv1.MaxUsageRecordsPerReport || size+len(value) > agentv1.MaxUsageReportBytes/2 {
				break
			}
			var record agentv1.UsageRecord
			if err := proto.Unmarshal(value, &record); err != nil {
				return fmt.Errorf("decode usage record %d: %w", binary.BigEndian.Uint64(key), err)
			}
			records = append(records, &record)
			size += len(value)
		}
		return nil
	})
	return records, lostThrough, err
}

// oldestUnacknowledged returns the start of the first unacknowledged record.
func (j *journal) oldestUnacknowledged() (time.Time, bool) {
	var start time.Time
	_ = j.db.View(func(tx *bbolt.Tx) error {
		value := tx.Bucket(recordsBucket).Get(sequenceKey(j.meta.AckedThrough + 1))
		var record agentv1.UsageRecord
		if value != nil && proto.Unmarshal(value, &record) == nil {
			start = record.GetStart().AsTime()
		}
		return nil
	})
	return start, !start.IsZero()
}

func (j *journal) health(rateBytesPerSecond float64) *agentv1.UsageProducerHealth {
	meta := j.meta
	health := &agentv1.UsageProducerHealth{
		JournalBytes:            meta.RecordBytes,
		JournalCapacityBytes:    j.capacity(),
		UnacknowledgedRecords:   meta.NextSequence - 1 - meta.AckedThrough,
		GapRecords:              meta.GapRecords,
		UnattributedEgressBytes: meta.Unattributed,
		LastError:               meta.LastError,
	}
	if oldest, ok := j.oldestUnacknowledged(); ok {
		health.OldestUnacknowledged = timestamppb.New(oldest)
	}
	if meta.ExhaustedSinceMS != 0 {
		health.ExhaustedSince = timestamppb.New(time.UnixMilli(meta.ExhaustedSinceMS))
	}
	if rateBytesPerSecond > 0 {
		health.OfflineCapacitySeconds = int64(float64(max(j.capacity()-meta.RecordBytes, 0)) / rateBytesPerSecond)
	}
	return health
}
