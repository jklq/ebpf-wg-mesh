package delivery

import (
	"slices"
	"strings"
	"sync"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"

	"google.golang.org/protobuf/proto"
)

// Incremental per-node allocation sync (2.10): checkpoint-plus-diff.
//
// The control plane remains authoritative for placement. The per-node
// allocation revision is the durable desired_revision (monotonic). Steady
// state sends bounded start/update/stop diffs ordered by that revision.
// Checkpoints (full DesiredNodeState) establish or repair the desired set on
// initialization, recovery, compaction, or cursor mismatch, but an unchanged
// reconnect sends nothing.
//
// Node config (peers, identities, subnets for now; 2.11/2.12 split further),
// pull credentials, and replica discovery are independently versioned content
// hashes, not allocation changes. They are sent only when their hash differs.

const (
	// MaxDiffEntriesPerAgent bounds retained diff history per node.
	MaxDiffEntriesPerAgent = 32
	// MaxDiffHistoryBytesPerAgent bounds retained history size per node.
	MaxDiffHistoryBytesPerAgent = 1024 * 1024
	// MaxDiffPayloadBytes caps a single diff payload; larger changes fall
	// back to a checkpoint.
	MaxDiffPayloadBytes = 256 * 1024
	// MaxDiffAllocationsPerMessage caps allocation changes per diff.
	MaxDiffAllocationsPerMessage = 100
)

// storedDiff is one retained allocation change from base to target.
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
}

// allocSync tracks per-agent diff history. It resets on live resign/become so
// a new live owner falls back to checkpoints rather than replaying lost diffs.
type allocSync struct {
	mu      sync.Mutex
	history map[string]*agentSyncHistory
}

func newAllocSync() *allocSync {
	return &allocSync{history: make(map[string]*agentSyncHistory)}
}

func (s *allocSync) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = make(map[string]*agentSyncHistory)
}

// recordAndDiff records current (post-sealed, final wire content without
// credentials) and returns diffs from base to current if available.
// It returns ok=false when a checkpoint is required (uninitialized base,
// compacted history, or gap).
func (s *allocSync) recordAndDiff(agentID string, current *agentv1.DesiredNodeState, base int64) (diffs []storedDiff, ok bool) {
	if s == nil || current == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.history == nil {
		s.history = make(map[string]*agentSyncHistory)
	}
	h, exists := s.history[agentID]
	if !exists {
		h = &agentSyncHistory{}
		s.history[agentID] = h
	}
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
		// Durable revision regressed; safest is a checkpoint and rebase.
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
		// Compact oldest until within caps.
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
	if base == target {
		return nil, true
	}
	if base < h.compactedBefore || base > target {
		return nil, false
	}
	// Collect contiguous diffs from base to target.
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
		if cursor == target {
			break
		}
	}
	if cursor != target {
		return nil, false
	}
	// Enforce per-payload caps; oversized diffs fall back to checkpoint.
	for _, d := range out {
		if d.SizeBytes > MaxDiffPayloadBytes || len(d.Starts)+len(d.Updates)+len(d.Stops) > MaxDiffAllocationsPerMessage {
			return nil, false
		}
	}
	return out, true
}

// recordCurrent records current as the latest for agentID. It is called on
// every DesiredStateForAgent load so history tracks durable bumps even when
// no agent is connected to observe intermediate revisions.
func (s *allocSync) recordCurrent(agentID string, current *agentv1.DesiredNodeState) {
	if s == nil || current == nil {
		return
	}
	_, _ = s.recordAndDiff(agentID, current, current.GetReconciliationCursor())
}

// diffsFrom returns retained diffs from base to the latest recorded revision.
// It returns ok=false when a checkpoint is required.
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
	target = h.lastRevision
	if base == target {
		return nil, target, true
	}
	if base < h.compactedBefore || base > target {
		return nil, target, false
	}
	var out []storedDiff
	cursor := base
	for _, d := range h.diffs {
		if d.Target <= cursor {
			continue
		}
		if d.Base != cursor {
			return nil, target, false
		}
		out = append(out, d)
		cursor = d.Target
		if cursor == target {
			break
		}
	}
	if cursor != target {
		return nil, target, false
	}
	for _, d := range out {
		if d.SizeBytes > MaxDiffPayloadBytes || len(d.Starts)+len(d.Updates)+len(d.Stops) > MaxDiffAllocationsPerMessage {
			return nil, target, false
		}
	}
	return out, target, true
}

// stripForDiff returns allocations+volumes without credentials, node config,
// or transport metadata for diff comparison.
func stripForDiff(state *agentv1.DesiredNodeState) *agentv1.DesiredNodeState {
	if state == nil {
		return &agentv1.DesiredNodeState{}
	}
	out := &agentv1.DesiredNodeState{
		AgentId:             state.GetAgentId(),
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

// Hash wrappers delegate to the shared reconciliation versions so control
// plane and agent compute identical content hashes.
func HashNodeConfig(config *agentv1.AssignedNodeConfig) string {
	return reconciliation.HashNodeConfig(config)
}

func HashCredentials(creds []*agentv1.AllocationCredential) string {
	return reconciliation.HashCredentials(creds)
}

func HashReplicas(addresses []string) string {
	return reconciliation.HashReplicas(addresses)
}

// InventoriesMatch checks hello allocations cover current desired IDs with
// matching desired generations, ignoring stopped extras. Unowned runtime
// containers are handled by runtime prune, not by allocation sync.
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
		// Last wins; hello carries one entry per allocation.
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
		// Non-stopped extra: could be a missed stop that did not bump the
		// cursor (should not happen) or a genuinely unowned durable entry.
		// Repair via checkpoint rather than silently ignoring.
		return false
	}
	return true
}

// ToProto converts stored diffs to wire messages (authority fields stamped by caller).
func (d storedDiff) ToProto(agentID string) *agentv1.AllocationDiff {
	out := &agentv1.AllocationDiff{
		AgentId: agentID, BaseRevision: d.Base, TargetRevision: d.Target,
		Stops: append([]string(nil), d.Stops...),
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
