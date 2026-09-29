package logpipeline

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// DropKey is the identity one gap row is keyed on.
type DropKey struct {
	ServiceID    string
	AllocationID string
	BuildID      string
	LogType      platformv1.ServiceLogType
	Stream       string
	Reason       string
}

// DropSet coalesces producer drop windows by identity: counts sum, the window
// widens, and every entry keeps a stable summary ID so at-least-once gap
// reports replace their row server-side. Take removes pending summaries for
// a send; Restore folds unsent ones back on failure. Not safe for concurrent use.
type DropSet struct {
	drops map[DropKey]*dropWindow
}

// dropWindow is one coalesced drop window.
type dropWindow struct {
	id     string
	count  uint64
	start  time.Time
	end    time.Time
	pinned bool
}

// NewSummaryID mints the stable identity kept across retries and expansions.
func NewSummaryID() string {
	var buf [16]byte
	// rand.Read never fails on supported platforms.
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func NewDropSet() *DropSet {
	return &DropSet{drops: make(map[DropKey]*dropWindow)}
}

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

// Summaries renders pending summaries in a stable order without emptying the set.
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

// Take empties the set, returning pending summaries in a stable order;
// Restore folds unsent ones back on failure.
func (s *DropSet) Take() []*platformv1.LogDropSummary {
	summaries := s.Summaries()
	if s != nil {
		s.drops = make(map[DropKey]*dropWindow)
	}
	return summaries
}

// Restore merges taken summaries back. On collision the restored entry wins:
// it was in flight and may be persisted server-side, while an entry created
// during the flight was never sent.
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

// addWindow folds one window into key's entry. The identity never changes
// once minted, except when id restores a previously sent lineage.
func (s *DropSet) addWindow(key DropKey, id string, count uint64, start, end time.Time) {
	if s == nil || count == 0 {
		return
	}
	if s.drops == nil {
		s.drops = make(map[DropKey]*dropWindow)
	}
	window, ok := s.drops[key]
	if !ok {
		window = &dropWindow{id: id, start: start, end: end, pinned: id != ""}
		if window.id == "" {
			window.id = NewSummaryID()
		}
		s.drops[key] = window
	} else if id != "" {
		// Keep the restored identity the server may already have stored.
		window.id = id
		window.pinned = true
	}
	window.count += count
	if start.Before(window.start) {
		window.start = start
	}
	if end.After(window.end) {
		window.end = end
	}
}

// Bound caps the set at max identities. Extras in one group (service, build,
// stream, log type, reason) fold into the largest survivor, keeping its
// summary ID; restored in-flight identities stay put. Groups never merge,
// so churn across tenants can remain above max.
func (s *DropSet) Bound(max int) {
	if s == nil || max < 1 || len(s.drops) <= max {
		return
	}
	type group struct {
		service string
		build   string
		stream  string
		reason  string
		logType platformv1.ServiceLogType
	}
	groups := make(map[group][]DropKey)
	for key := range s.drops {
		g := group{key.ServiceID, key.BuildID, key.Stream, key.Reason, key.LogType}
		groups[g] = append(groups[g], key)
	}
	for _, keys := range groups {
		if len(s.drops) <= max {
			return
		}
		survivor := keys[0]
		for _, key := range keys[1:] {
			if preferDropSurvivor(s.drops[key], key, s.drops[survivor], survivor) {
				survivor = key
			}
		}
		dst := s.drops[survivor]
		for _, key := range keys {
			if key == survivor || len(s.drops) <= max {
				continue
			}
			src := s.drops[key]
			if src.pinned {
				continue
			}
			dst.count += src.count
			if src.start.Before(dst.start) {
				dst.start = src.start
			}
			if src.end.After(dst.end) {
				dst.end = src.end
			}
			delete(s.drops, key)
		}
	}
}

func preferDropSurvivor(candidate *dropWindow, candidateKey DropKey, current *dropWindow, currentKey DropKey) bool {
	if candidate.pinned != current.pinned {
		return candidate.pinned
	}
	if candidate.count != current.count {
		return candidate.count > current.count
	}
	return compareDropKeys(candidateKey, currentKey) < 0
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

// PendingDropsFile holds unreported drop summaries next to the spool so they
// report after a restart, or after a retried builder attempt takes them over.
const PendingDropsFile = "pending-drops.json"

// persistedDrop is the durable form of one pending drop summary.
type persistedDrop struct {
	ServiceID    string    `json:"service_id"`
	AllocationID string    `json:"allocation_id"`
	BuildID      string    `json:"build_id"`
	LogType      int32     `json:"log_type"`
	Stream       string    `json:"stream"`
	DroppedCount uint64    `json:"dropped_count"`
	Reason       string    `json:"reason"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	SummaryID    string    `json:"summary_id"`
}

// LoadDrops reads summaries persisted by an earlier process; a missing file yields none.
func LoadDrops(dir string) ([]*platformv1.LogDropSummary, error) {
	raw, err := os.ReadFile(filepath.Join(dir, PendingDropsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []persistedDrop
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]*platformv1.LogDropSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, &platformv1.LogDropSummary{
			ServiceId:    row.ServiceID,
			AllocationId: row.AllocationID,
			BuildId:      row.BuildID,
			LogType:      platformv1.ServiceLogType(row.LogType),
			Stream:       row.Stream,
			DroppedCount: row.DroppedCount,
			Reason:       row.Reason,
			WindowStart:  timestamppb.New(row.WindowStart),
			WindowEnd:    timestamppb.New(row.WindowEnd),
			SummaryId:    row.SummaryID,
		})
	}
	return out, nil
}

// SaveDrops snapshots summaries next to the spool. The snapshot is advisory:
// retries collapse server-side by gap identity, so a stale copy only re-reports.
func SaveDrops(dir string, summaries []*platformv1.LogDropSummary) error {
	rows := make([]persistedDrop, 0, len(summaries))
	for _, summary := range summaries {
		rows = append(rows, persistedDrop{
			ServiceID:    summary.GetServiceId(),
			AllocationID: summary.GetAllocationId(),
			BuildID:      summary.GetBuildId(),
			LogType:      int32(summary.GetLogType()),
			Stream:       summary.GetStream(),
			DroppedCount: summary.GetDroppedCount(),
			Reason:       summary.GetReason(),
			WindowStart:  summary.GetWindowStart().AsTime(),
			WindowEnd:    summary.GetWindowEnd().AsTime(),
			SummaryID:    summary.GetSummaryId(),
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encode pending log drop summaries: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".drops-*.json")
	if err != nil {
		return fmt.Errorf("stage pending log drop summaries: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write pending log drop summaries: %w", err)
	}
	// Drop summaries are the only record of shed lines: sync the snapshot
	// and its directory entry, like the spool cursor.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync pending log drop summaries: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close pending log drop summaries: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, PendingDropsFile)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit pending log drop summaries: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("commit pending log drop summaries: %w", err)
	}
	return nil
}
