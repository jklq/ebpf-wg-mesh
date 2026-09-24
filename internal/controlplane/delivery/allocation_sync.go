package delivery

import (
	"slices"
	"strings"
	"sync"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/proto"
)

// Incremental per-node allocation sync: checkpoint-plus-diff over the
// monotonic per-node allocation revision. Diffs are an optimization, never a
// correctness requirement: any gap, compaction, or regression falls back to
// a full checkpoint.

const (
	MaxDiffEntriesPerAgent      = 32
	MaxDiffHistoryBytesPerAgent = 1024 * 1024
	// A diff above these caps falls back to a checkpoint.
	MaxDiffPayloadBytes          = 256 * 1024
	MaxDiffAllocationsPerMessage = 100
	// Evicting an agent only costs it one checkpoint delivery.
	MaxTrackedAgents = 512
)

type storedDiff struct {
	Base         int64
	Target       int64
	Starts       []*agentv1.DesiredService
	Updates      []*agentv1.DesiredService
	Stops        []string
	VolumeStarts []*agentv1.DesiredVolume
	VolumeStops  []string
	SizeBytes    int
}

type agentSyncHistory struct {
	lastRevision int64
	lastSnapshot *agentv1.DesiredNodeState // stripped allocations+volumes only
	diffs        []storedDiff
	historyBytes int
	// compactedBefore is the lowest retained base; cursors below need a checkpoint.
	compactedBefore int64
	initialized     bool
	used            uint64 // LRU order for MaxTrackedAgents eviction
}

// allocSync resets on live resign/become so a new live owner falls back to
// checkpoints rather than replaying diffs it never saw.
type allocSync struct {
	mu      sync.Mutex
	history map[string]*agentSyncHistory
	use     uint64
}

func newAllocSync() *allocSync {
	return &allocSync{history: make(map[string]*agentSyncHistory)}
}

// historyFor creates agentID's history with LRU eviction. Callers hold s.mu.
func (s *allocSync) historyFor(agentID string) *agentSyncHistory {
	h := s.history[agentID]
	if h == nil && len(s.history) >= MaxTrackedAgents {
		var oldestKey string
		var oldest *agentSyncHistory
		for key, candidate := range s.history {
			if oldest == nil || candidate.used < oldest.used {
				oldestKey, oldest = key, candidate
			}
		}
		delete(s.history, oldestKey)
	}
	if h == nil {
		h = &agentSyncHistory{}
	}
	s.use++
	h.used = s.use
	s.history[agentID] = h
	return h
}

func (s *allocSync) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = make(map[string]*agentSyncHistory)
}

// recordAndDiff records current and returns diffs from base to it.
// ok=false when a checkpoint is required (uninitialized base, compacted
// history, or gap).
func (s *allocSync) recordAndDiff(agentID string, current *agentv1.DesiredNodeState, base int64) (diffs []storedDiff, ok bool) {
	if s == nil || current == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.historyFor(agentID)
	stripped := stripForDiff(current)
	target := current.GetReconciliationCursor()
	if !h.initialized {
		h.lastRevision = target
		h.lastSnapshot = stripped
		h.compactedBefore = target
		h.initialized = true
		if base == target {
			return nil, true
		}
		return nil, false
	}
	if target < h.lastRevision {
		// Durable revision regressed; rebase and require a checkpoint.
		h.lastRevision = target
		h.lastSnapshot = stripped
		h.diffs = nil
		h.historyBytes = 0
		h.compactedBefore = target
		return nil, false
	}
	if target > h.lastRevision {
		diff := diffSnapshots(h.lastSnapshot, stripped, h.lastRevision, target)
		h.diffs = append(h.diffs, diff)
		h.historyBytes += diff.SizeBytes
		h.lastRevision = target
		h.lastSnapshot = stripped
		for len(h.diffs) > 0 && (len(h.diffs) > MaxDiffEntriesPerAgent || h.historyBytes > MaxDiffHistoryBytesPerAgent) {
			h.historyBytes -= h.diffs[0].SizeBytes
			h.diffs = h.diffs[1:]
		}
		if len(h.diffs) > 0 {
			h.compactedBefore = h.diffs[0].Base
		} else {
			h.compactedBefore = h.lastRevision
		}
	}
	return h.diffsFromBase(base)
}

// diffsFromBase collects the contiguous retained chain from base to the
// latest revision, within the per-payload caps. ok=false when a checkpoint
// is required.
func (h *agentSyncHistory) diffsFromBase(base int64) (diffs []storedDiff, ok bool) {
	if base == h.lastRevision {
		return nil, true
	}
	if base < h.compactedBefore || base > h.lastRevision {
		return nil, false
	}
	var out []storedDiff
	cursor := base
	for _, d := range h.diffs {
		if d.Target <= cursor {
			continue
		}
		if d.Base != cursor {
			return nil, false
		}
		out = append(out, d)
		cursor = d.Target
		if cursor == h.lastRevision {
			break
		}
	}
	if cursor != h.lastRevision {
		return nil, false
	}
	for _, d := range out {
		if d.SizeBytes > MaxDiffPayloadBytes || len(d.Starts)+len(d.Updates)+len(d.Stops) > MaxDiffAllocationsPerMessage {
			return nil, false
		}
	}
	return out, true
}

// recordCurrent tracks durable revisions even when no agent is connected to
// observe the intermediate bumps.
func (s *allocSync) recordCurrent(agentID string, current *agentv1.DesiredNodeState) {
	if s == nil || current == nil {
		return
	}
	_, _ = s.recordAndDiff(agentID, current, current.GetReconciliationCursor())
}

// rebase moves the diff baseline to current after a checkpoint, which
// travels outside the retained diff chain. Without this the next diff would
// compare against a stale baseline and silently skip fields the checkpoint
// changed.
func (s *allocSync) rebase(agentID string, current *agentv1.DesiredNodeState) {
	if s == nil || current == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.historyFor(agentID)
	h.lastRevision = current.GetReconciliationCursor()
	h.lastSnapshot = stripForDiff(current)
	h.initialized = true
}

func (s *allocSync) diffsFrom(agentID string, base int64) (diffs []storedDiff, target int64, ok bool) {
	if s == nil {
		return nil, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, exists := s.history[agentID]
	if !exists || !h.initialized {
		return nil, 0, false
	}
	s.use++
	h.used = s.use
	out, ok := h.diffsFromBase(base)
	return out, h.lastRevision, ok
}

// stripForDiff keeps allocations+volumes only; credentials, node config, and
// transport metadata are excluded from diff comparison.
func stripForDiff(state *agentv1.DesiredNodeState) *agentv1.DesiredNodeState {
	if state == nil {
		return &agentv1.DesiredNodeState{}
	}
	out := &agentv1.DesiredNodeState{
		AgentId:              state.GetAgentId(),
		ReconciliationCursor: state.GetReconciliationCursor(),
	}
	for _, v := range state.GetVolumes() {
		out.Volumes = append(out.Volumes, proto.Clone(v).(*agentv1.DesiredVolume))
	}
	for _, svc := range state.GetServices() {
		clean := proto.Clone(svc).(*agentv1.DesiredService)
		clean.RegistryUsername = ""
		clean.RegistryPassword = ""
		out.Services = append(out.Services, clean)
	}
	sortDesiredForDiff(out)
	return out
}

func sortDesiredForDiff(state *agentv1.DesiredNodeState) {
	slices.SortFunc(state.Volumes, func(a, b *agentv1.DesiredVolume) int {
		return strings.Compare(a.GetVolumeId(), b.GetVolumeId())
	})
	slices.SortFunc(state.Services, func(a, b *agentv1.DesiredService) int {
		return strings.Compare(a.GetAllocationId(), b.GetAllocationId())
	})
}

func diffSnapshots(old, new *agentv1.DesiredNodeState, base, target int64) storedDiff {
	if old == nil {
		old = &agentv1.DesiredNodeState{}
	}
	if new == nil {
		new = &agentv1.DesiredNodeState{}
	}
	oldServices := make(map[string]*agentv1.DesiredService, len(old.GetServices()))
	for _, svc := range old.GetServices() {
		oldServices[svc.GetAllocationId()] = svc
	}
	newServices := make(map[string]*agentv1.DesiredService, len(new.GetServices()))
	for _, svc := range new.GetServices() {
		newServices[svc.GetAllocationId()] = svc
	}
	var starts, updates []*agentv1.DesiredService
	var stops []string
	for id, svc := range newServices {
		prev, exists := oldServices[id]
		if !exists {
			starts = append(starts, proto.Clone(svc).(*agentv1.DesiredService))
			continue
		}
		if !proto.Equal(prev, svc) {
			updates = append(updates, proto.Clone(svc).(*agentv1.DesiredService))
		}
	}
	for id := range oldServices {
		if _, exists := newServices[id]; !exists {
			stops = append(stops, id)
		}
	}
	slices.SortFunc(starts, func(a, b *agentv1.DesiredService) int {
		return strings.Compare(a.GetAllocationId(), b.GetAllocationId())
	})
	slices.SortFunc(updates, func(a, b *agentv1.DesiredService) int {
		return strings.Compare(a.GetAllocationId(), b.GetAllocationId())
	})
	slices.Sort(stops)

	oldVolumes := make(map[string]*agentv1.DesiredVolume, len(old.GetVolumes()))
	for _, v := range old.GetVolumes() {
		oldVolumes[v.GetVolumeId()] = v
	}
	newVolumes := make(map[string]*agentv1.DesiredVolume, len(new.GetVolumes()))
	for _, v := range new.GetVolumes() {
		newVolumes[v.GetVolumeId()] = v
	}
	var volumeStarts []*agentv1.DesiredVolume
	var volumeStops []string
	for id, v := range newVolumes {
		prev, exists := oldVolumes[id]
		if !exists || !proto.Equal(prev, v) {
			volumeStarts = append(volumeStarts, proto.Clone(v).(*agentv1.DesiredVolume))
		}
	}
	for id := range oldVolumes {
		if _, exists := newVolumes[id]; !exists {
			volumeStops = append(volumeStops, id)
		}
	}
	slices.SortFunc(volumeStarts, func(a, b *agentv1.DesiredVolume) int {
		return strings.Compare(a.GetVolumeId(), b.GetVolumeId())
	})
	slices.Sort(volumeStops)

	diff := storedDiff{
		Base: base, Target: target,
		Starts: starts, Updates: updates, Stops: stops,
		VolumeStarts: volumeStarts, VolumeStops: volumeStops,
	}
	diff.SizeBytes = diffPayloadSize(&diff)
	return diff
}

func diffPayloadSize(d *storedDiff) int {
	if d == nil {
		return 0
	}
	size := 0
	for _, svc := range d.Starts {
		size += proto.Size(svc)
	}
	for _, svc := range d.Updates {
		size += proto.Size(svc)
	}
	for _, id := range d.Stops {
		size += len(id) + 8
	}
	for _, v := range d.VolumeStarts {
		size += proto.Size(v)
	}
	for _, id := range d.VolumeStops {
		size += len(id) + 8
	}
	return size
}

// InventoriesMatch checks hello allocations cover current desired IDs with
// matching generations, ignoring stopped extras. Unowned runtime containers
// are handled by runtime prune, not allocation sync.
func InventoriesMatch(hello []*agentv1.ServiceCondition, current []*agentv1.DesiredService) bool {
	desired := make(map[string]*agentv1.DesiredService, len(current))
	for _, svc := range current {
		desired[svc.GetAllocationId()] = svc
	}
	seen := make(map[string]*agentv1.ServiceCondition, len(hello))
	for _, cond := range hello {
		id := cond.GetAllocationId()
		if id == "" {
			continue
		}
		seen[id] = cond
	}
	for id, svc := range desired {
		cond, ok := seen[id]
		if !ok {
			return false
		}
		if cond.GetDesiredSpecRevision() != svc.GetDesiredSpecRevision() ||
			cond.GetDesiredRolloutGeneration() != svc.GetDesiredRolloutGeneration() {
			return false
		}
	}
	for id, cond := range seen {
		if _, ok := desired[id]; ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(cond.GetPhase()), "Stopped") {
			continue
		}
		// A non-stopped extra repairs via checkpoint rather than silently
		// ignoring a possible missed stop.
		return false
	}
	return true
}

// ToProto converts to the wire message; authority fields are stamped by the caller.
func (d storedDiff) ToProto(agentID string) *agentv1.AllocationDiff {
	out := &agentv1.AllocationDiff{
		AgentId: agentID, BaseRevision: d.Base, TargetRevision: d.Target,
		Stops:       append([]string(nil), d.Stops...),
		VolumeStops: append([]string(nil), d.VolumeStops...),
	}
	for _, svc := range d.Starts {
		out.Starts = append(out.Starts, proto.Clone(svc).(*agentv1.DesiredService))
	}
	for _, svc := range d.Updates {
		out.Updates = append(out.Updates, proto.Clone(svc).(*agentv1.DesiredService))
	}
	for _, v := range d.VolumeStarts {
		out.VolumeStarts = append(out.VolumeStarts, proto.Clone(v).(*agentv1.DesiredVolume))
	}
	return out
}
