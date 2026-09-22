package logpipeline

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// DropKey identifies one gap summary identity — the fields a gap row
// is keyed on.
type DropKey struct {
	ServiceID    string
	AllocationID string
	BuildID      string
	LogType      platformv1.ServiceLogType
	Stream       string
	Reason       string
}

// DropSet coalesces producer drop windows by identity: counts sum and
// the covered window widens, so the set stays bounded at one entry
// per identity however long a backend outage lasts. A send Takes the
// pending summaries and Restores them on failure, keeping accounting
// exact while new drops keep folding in. Every entry carries a stable
// summary ID preserved across retries and growth, so at-least-once
// gap reports replace their row server-side instead of
// double-counting. It is not safe for concurrent use; callers
// serialize access.
type DropSet struct {
	drops map[DropKey]*dropWindow
}

// dropWindow is one coalesced drop window: a summed count over the
// covered time range under a stable identity.
type dropWindow struct {
	id    string
	count uint64
	start time.Time
	end   time.Time
}

// NewSummaryID mints a stable identity for one coalesced drop
// lineage. Retries and expansions keep it.
func NewSummaryID() string {
	var buf [16]byte
	// rand.Read never fails on supported platforms.
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// NewDropSet builds an empty drop set.
func NewDropSet() *DropSet {
	return &DropSet{drops: make(map[DropKey]*dropWindow)}
}

// Add folds one drop window into the entry for key.
func (s *DropSet) Add(key DropKey, count uint64, start, end time.Time) {
	s.addWindow(key, "", count, start, end)
}

// Len reports distinct pending identities.
func (s *DropSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.drops)
}

// Summaries renders the pending set as wire summaries in a stable
// order without emptying it.
func (s *DropSet) Summaries() []*platformv1.LogDropSummary {
	if s == nil {
		return nil
	}
	keys := make([]DropKey, 0, len(s.drops))
	for key := range s.drops {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, compareDropKeys)
	summaries := make([]*platformv1.LogDropSummary, 0, len(keys))
	for _, key := range keys {
		window := s.drops[key]
		summaries = append(summaries, &platformv1.LogDropSummary{
			ServiceId:    key.ServiceID,
			AllocationId: key.AllocationID,
			BuildId:      key.BuildID,
			LogType:      key.LogType,
			Stream:       key.Stream,
			DroppedCount: window.count,
			Reason:       key.Reason,
			WindowStart:  timestamppb.New(window.start),
			WindowEnd:    timestamppb.New(window.end),
			SummaryId:    window.id,
		})
	}
	return summaries
}

// Take empties the set and returns the pending summaries in a stable
// order. A send takes them out; Restore folds unsent ones back so a
// failed send keeps its accounting exact.
func (s *DropSet) Take() []*platformv1.LogDropSummary {
	summaries := s.Summaries()
	if s != nil {
		s.drops = make(map[DropKey]*dropWindow)
	}
	return summaries
}

// Restore merges previously taken summaries back into the set. The
// restored identity wins on collision: the restored entry was in
// flight and may already be persisted server-side, while an entry
// created during the flight cannot have been sent yet.
func (s *DropSet) Restore(summaries []*platformv1.LogDropSummary) {
	for _, summary := range summaries {
		if summary == nil {
			continue
		}
		s.addWindow(DropKey{
			ServiceID:    summary.GetServiceId(),
			AllocationID: summary.GetAllocationId(),
			BuildID:      summary.GetBuildId(),
			LogType:      summary.GetLogType(),
			Stream:       summary.GetStream(),
			Reason:       summary.GetReason(),
		}, summary.GetSummaryId(), summary.GetDroppedCount(),
			summary.GetWindowStart().AsTime(), summary.GetWindowEnd().AsTime())
	}
}

// addWindow folds one drop window into the entry for key, optionally
// under an existing stable identity. The identity never changes once
// minted unless id names a previously sent lineage being restored.
func (s *DropSet) addWindow(key DropKey, id string, count uint64, start, end time.Time) {
	if s == nil || count == 0 {
		return
	}
	if s.drops == nil {
		s.drops = make(map[DropKey]*dropWindow)
	}
	window, ok := s.drops[key]
	if !ok {
		window = &dropWindow{id: id, start: start, end: end}
		if window.id == "" {
			window.id = NewSummaryID()
		}
		s.drops[key] = window
	} else if id != "" {
		window.id = id
	}
	window.count += count
	if start.Before(window.start) {
		window.start = start
	}
	if end.After(window.end) {
		window.end = end
	}
}

func compareDropKeys(a, b DropKey) int {
	return cmp.Or(
		strings.Compare(a.AllocationID, b.AllocationID),
		strings.Compare(a.BuildID, b.BuildID),
		strings.Compare(a.ServiceID, b.ServiceID),
		cmp.Compare(a.LogType, b.LogType),
		strings.Compare(a.Stream, b.Stream),
		strings.Compare(a.Reason, b.Reason),
	)
}
