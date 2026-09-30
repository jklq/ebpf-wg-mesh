package logpipeline

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var dropsBucket = []byte("drops")
var dropKeysBucket = []byte("drop_keys")

// DropKey preserves the exact owner and stream of a loss; identities never
// fold across allocations or services merely to meet an in-memory key cap.
type DropKey struct {
	ServiceID    string
	AllocationID string
	BuildID      string
	LogType      platformv1.ServiceLogType
	Stream       string
	Reason       string
}

// Drop is one addition to a coalesced, durably reported loss window.
type Drop struct {
	Key        DropKey
	Count      uint64
	Start, End time.Time
}

func NewSummaryID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func summaryKey(summary *platformv1.LogDropSummary) []byte {
	raw, _ := json.Marshal(DropKey{summary.GetServiceId(), summary.GetAllocationId(), summary.GetBuildId(), summary.GetLogType(), summary.GetStream(), summary.GetReason()})
	return raw
}
func putSummary(tx *bolt.Tx, summary *platformv1.LogDropSummary) error {
	raw, err := proto.Marshal(summary)
	if err != nil {
		return err
	}
	return tx.Bucket(dropsBucket).Put([]byte(summary.GetSummaryId()), raw)
}
func readSummary(raw []byte) (*platformv1.LogDropSummary, error) {
	var summary platformv1.LogDropSummary
	err := proto.Unmarshal(raw, &summary)
	return &summary, err
}

func addDrop(tx *bolt.Tx, drop Drop) error {
	if drop.Count == 0 {
		return nil
	}
	summary := &platformv1.LogDropSummary{
		ServiceId: drop.Key.ServiceID, AllocationId: drop.Key.AllocationID, BuildId: drop.Key.BuildID,
		LogType: drop.Key.LogType, Stream: drop.Key.Stream, Reason: drop.Key.Reason,
		WindowStart: timestamppb.New(drop.Start), WindowEnd: timestamppb.New(drop.End), DroppedCount: drop.Count,
	}
	key := summaryKey(summary)
	index := tx.Bucket(dropKeysBucket)
	if id := index.Get(key); id != nil {
		current, err := readSummary(tx.Bucket(dropsBucket).Get(id))
		if err != nil {
			return err
		}
		current.DroppedCount += drop.Count
		if drop.Start.Before(current.GetWindowStart().AsTime()) {
			current.WindowStart = summary.WindowStart
		}
		if drop.End.After(current.GetWindowEnd().AsTime()) {
			current.WindowEnd = summary.WindowEnd
		}
		return putSummary(tx, current)
	}
	summary.SummaryId = NewSummaryID()
	if err := putSummary(tx, summary); err != nil {
		return err
	}
	return index.Put(key, []byte(summary.SummaryId))
}

// AddDrops persists producer losses immediately. A storage error leaves the
// previous accounting unchanged and must be surfaced by the producer.
func (s *Spool) AddDrops(drops ...Drop) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return spoolError(s.db.Update(func(tx *bolt.Tx) error {
		for _, drop := range drops {
			if err := addDrop(tx, drop); err != nil {
				return err
			}
		}
		return nil
	}))
}

// PendingDrops snapshots durable summaries without taking them out of storage.
// Failed sends need no restore operation; the same IDs survive process restart.
func (s *Spool) PendingDrops() ([]*platformv1.LogDropSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var summaries []*platformv1.LogDropSummary
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(dropsBucket).ForEach(func(_, raw []byte) error {
			summary, err := readSummary(raw)
			if err != nil {
				return err
			}
			summaries = append(summaries, summary)
			return nil
		})
	})
	return summaries, err
}

func acknowledgeDrops(tx *bolt.Tx, snapshot []*platformv1.LogDropSummary) error {
	bucket := tx.Bucket(dropsBucket)
	index := tx.Bucket(dropKeysBucket)
	for _, acknowledged := range snapshot {
		id := []byte(acknowledged.GetSummaryId())
		raw := bucket.Get(id)
		if raw == nil {
			return errors.New("stale log drop acknowledgement")
		}
		current, err := readSummary(raw)
		if err != nil {
			return err
		}
		if current.GetDroppedCount() < acknowledged.GetDroppedCount() {
			return errors.New("log drop acknowledgement exceeds pending count")
		}
		key := summaryKey(current)
		if err := bucket.Delete(id); err != nil {
			return err
		}
		if string(index.Get(key)) == string(id) {
			if err := index.Delete(key); err != nil {
				return err
			}
		}
		if current.DroppedCount > acknowledged.GetDroppedCount() {
			// Losses arriving during the send get a fresh lineage after the
			// acknowledged snapshot; otherwise its reduced total would overwrite
			// the already delivered count at the sink.
			current.DroppedCount -= acknowledged.GetDroppedCount()
			current.SummaryId = NewSummaryID()
			if err := putSummary(tx, current); err != nil {
				return err
			}
			if index.Get(key) == nil {
				if err := index.Put(key, []byte(current.SummaryId)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// AcknowledgeDrops consumes a durable snapshot after a build attempt takeover.
// Normal delivery acknowledges drops together with the record cursor in Commit.
func (s *Spool) AcknowledgeDrops(snapshot []*platformv1.LogDropSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return spoolError(s.db.Update(func(tx *bolt.Tx) error { return acknowledgeDrops(tx, snapshot) }))
}

// MergeDrops imports a dead attempt's summaries with their existing IDs.
// Repeating a takeover after a crash replaces the snapshot instead of adding
// its count again. The source is acknowledged only after this transaction commits.
func (s *Spool) MergeDrops(snapshot []*platformv1.LogDropSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return spoolError(s.db.Update(func(tx *bolt.Tx) error {
		for _, summary := range snapshot {
			if summary.GetSummaryId() == "" {
				return errors.New("inherited log drop has no stable identity")
			}
			if raw := tx.Bucket(dropsBucket).Get([]byte(summary.SummaryId)); raw != nil {
				current, err := readSummary(raw)
				if err != nil {
					return err
				}
				if current.GetDroppedCount() >= summary.GetDroppedCount() {
					continue
				}
			}
			if err := putSummary(tx, summary); err != nil {
				return err
			}
			key := summaryKey(summary)
			if tx.Bucket(dropKeysBucket).Get(key) == nil {
				if err := tx.Bucket(dropKeysBucket).Put(key, []byte(summary.SummaryId)); err != nil {
					return err
				}
			}
		}
		return nil
	}))
}
