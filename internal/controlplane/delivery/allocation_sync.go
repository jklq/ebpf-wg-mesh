package delivery

import (
	"crypto/sha256"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
	"maps"
	"slices"
	"strings"
	"sync"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/proto"
)

// Incremental per-node allocation sync: checkpoint-plus-diff over the monotonic
// per-node revision. Diffs are an optimization; any gap falls back to a
// full checkpoint.

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

// Only per-allocation fingerprints and bounded wire patches are retained. The
// shared product projection owns the rows; a journal batch invalidates exactly
// the assignments whose rendered desired state can change.
type allocationFingerprint struct {
	Content [32]byte
	Overlay string
}

type agentSyncHistory struct {
	revision     int64
	target       int64
	services     map[string]allocationFingerprint
	volumes      map[string][32]byte
	dirty        map[string]bool
	serial       uint64
	diffs        []storedDiff
	historyBytes int
	initialized  bool
	used         uint64
}

type syncBaseline struct {
	history  *agentSyncHistory
	revision int64
	services map[string]allocationFingerprint
	volumes  map[string][32]byte // immutable fingerprint maps
	dirty    []string
	serial   uint64
	usable   bool
}

type allocSync struct {
	mu      sync.Mutex
	history map[string]*agentSyncHistory
	use     uint64
}

func newAllocSync() *allocSync { return &allocSync{history: make(map[string]*agentSyncHistory)} }

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
		h = &agentSyncHistory{dirty: make(map[string]bool)}
		s.history[agentID] = h
	}
	s.use++
	h.used = s.use
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

func (s *allocSync) applied(before *journal.Projection, update journal.Applied) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	after := update.Projection
	if update.Reset {
		for id, h := range s.history {
			h.initialized = false
			h.services = nil
			h.volumes = nil
			h.diffs = nil
			h.historyBytes = 0
			h.dirty = make(map[string]bool)
			h.target = after.Agents[id].DesiredRevision
			h.serial++
		}
		return
	}
	// Group effects by their old/new host once. Unrelated commits do not touch a
	// history or invalidate a sync plan currently being rendered for another node.
	byAgent := make(map[string]map[string]bool)
	for id := range changedAssignmentIDs(before, after, update.Batches) {
		for _, agentID := range []string{before.Assignments[id].AgentID, after.Assignments[id].AgentID} {
			if agentID == "" {
				continue
			}
			if byAgent[agentID] == nil {
				byAgent[agentID] = make(map[string]bool)
			}
			byAgent[agentID][id] = true
		}
	}
	for _, batch := range update.Batches {
		for _, c := range batch.Agents {
			if before.Agents[c.Key].DesiredRevision != after.Agents[c.Key].DesiredRevision && byAgent[c.Key] == nil {
				byAgent[c.Key] = make(map[string]bool)
			}
		}
	}
	for agentID, ids := range byAgent {
		h := s.history[agentID]
		if h == nil {
			continue
		}
		target := after.Agents[agentID].DesiredRevision
		if h.target == target && len(ids) == 0 {
			continue
		}
		h.target = target
		h.serial++
		if !h.initialized {
			continue
		}
		for id := range ids {
			h.dirty[id] = true
		}
		// Disconnected agents can accumulate arbitrarily many removed identities.
		// Beyond a useful patch size, drop the optimization and require a checkpoint.
		if len(h.dirty) > MaxDiffAllocationsPerMessage {
			h.initialized = false
			h.services = nil
			h.volumes = nil
			h.diffs = nil
			h.historyBytes = 0
			h.dirty = make(map[string]bool)
		}
	}
}

func changedAssignmentIDs(before, after *journal.Projection, batches []journal.Batch) map[string]bool {
	ids, services, environments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, batch := range batches {
		for _, c := range batch.Assignments {
			ids[c.Key] = true
		}
		for _, c := range batch.Services {
			services[c.Key] = true
		}
		for _, c := range batch.Revisions {
			id, _, _ := strings.Cut(c.Key, "/")
			services[id] = true
		}
		for _, c := range batch.Rollouts {
			id, _, _ := strings.Cut(c.Key, "/")
			services[id] = true
		}
		for _, c := range batch.Domains {
			services[before.Domains[c.Key].ServiceID] = true
			services[after.Domains[c.Key].ServiceID] = true
		}
		for _, c := range batch.Environments {
			environments[c.Key] = true
		}
		for _, c := range batch.Volumes {
			environments[before.Volumes[c.Key].EnvironmentID] = true
			environments[after.Volumes[c.Key].EnvironmentID] = true
		}
		for _, c := range batch.Projects {
			for _, env := range before.EnvironmentIDsForProject(c.Key) {
				environments[env] = true
			}
			for _, env := range after.EnvironmentIDsForProject(c.Key) {
				environments[env] = true
			}
		}
	}
	for env := range environments {
		for _, id := range before.ServiceIDsForEnvironment(env) {
			services[id] = true
		}
		for _, id := range after.ServiceIDsForEnvironment(env) {
			services[id] = true
		}
	}
	for service := range services {
		for _, id := range before.AssignmentIDsForService(service) {
			ids[id] = true
		}
		for _, id := range after.AssignmentIDsForService(service) {
			ids[id] = true
		}
	}
	return ids
}

func (s *allocSync) baseline(agentID string, cursor int64) syncBaseline {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.historyFor(agentID)
	ids := slices.Collect(maps.Keys(h.dirty))
	slices.Sort(ids)
	return syncBaseline{h, h.revision, h.services, h.volumes, ids, h.serial, h.initialized && h.target <= cursor && h.revision <= cursor}
}

// acceptDiff publishes rendered patches only if their captured invalidation
// prefix still matches. A concurrent command makes the caller send its captured
// checkpoint instead; the pending newer effects remain for the next sync.
func (s *allocSync) acceptDiff(agentID string, baseline syncBaseline, cursor int64, services map[string]allocationFingerprint, volumes map[string][32]byte, diff storedDiff) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.history[agentID]
	if h == nil || h != baseline.history || h.serial != baseline.serial || !h.initialized || h.revision != baseline.revision {
		return false
	}
	h.revision, h.services, h.volumes = cursor, services, volumes
	h.dirty = make(map[string]bool)
	h.diffs = append(h.diffs, diff)
	h.historyBytes += diff.SizeBytes
	for len(h.diffs) > 0 && (len(h.diffs) > MaxDiffEntriesPerAgent || h.historyBytes > MaxDiffHistoryBytesPerAgent) {
		h.historyBytes -= h.diffs[0].SizeBytes
		h.diffs = h.diffs[1:]
	}
	return true
}

func (s *allocSync) rebase(agentID string, current *agentv1.DesiredNodeState) {
	if s == nil || current == nil {
		return
	}
	services, volumes := fingerprintState(current)
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.historyFor(agentID)
	cursor := current.GetReconciliationCursor()
	// A delayed checkpoint callback must not rewind a newer established baseline.
	if h.initialized && cursor < h.revision {
		return
	}
	h.revision, h.services, h.volumes = cursor, services, volumes
	if h.target < cursor {
		h.target = cursor
	}
	h.initialized = true
	h.diffs = nil
	h.historyBytes = 0
	if h.target <= cursor {
		h.dirty = make(map[string]bool)
	}
	h.serial++
}

func fingerprintState(state *agentv1.DesiredNodeState) (map[string]allocationFingerprint, map[string][32]byte) {
	services, volumes := map[string]allocationFingerprint{}, map[string][32]byte{}
	for _, svc := range state.GetServices() {
		services[svc.GetAllocationId()] = serviceFingerprint(svc)
	}
	for _, v := range state.GetVolumes() {
		volumes[v.GetVolumeId()] = fingerprint(v)
	}
	return services, volumes
}

func serviceFingerprint(svc *agentv1.DesiredService) allocationFingerprint {
	return allocationFingerprint{Content: fingerprint(svc),
		Overlay: reconciliation.HashObservationOverlay([]*agentv1.DesiredService{svc})}
}

func fingerprint(message proto.Message) [32]byte {
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	return sha256.Sum256(raw)
}

func (s *allocSync) diffsFrom(agentID string, base int64) (diffs []storedDiff, target int64, ok bool) {
	if s == nil {
		return nil, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.history[agentID]
	if h == nil || !h.initialized {
		return nil, 0, false
	}
	s.use++
	h.used = s.use
	diffs, ok = h.diffsFromBase(base)
	return diffs, h.revision, ok
}

func (h *agentSyncHistory) diffsFromBase(base int64) ([]storedDiff, bool) {
	if base == h.revision {
		return nil, true
	}
	if base > h.revision {
		return nil, false
	}
	var out []storedDiff
	cursor := base
	for _, d := range h.diffs {
		if d.Target <= cursor {
			continue
		}
		if d.Base != cursor || d.SizeBytes > MaxDiffPayloadBytes || len(d.Starts)+len(d.Updates)+len(d.Stops) > MaxDiffAllocationsPerMessage {
			return nil, false
		}
		out = append(out, d)
		cursor = d.Target
		if cursor == h.revision {
			return out, true
		}
	}
	return nil, false
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
// matching generations, ignoring stopped extras.
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
		// A non-stopped extra repairs via checkpoint, never silent ignore.
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

func (s *allocSync) discardThrough(agentID string, cursor int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.history[agentID]; h != nil && h.revision <= cursor {
		delete(s.history, agentID)
	}
}
