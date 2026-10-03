package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
)

var ErrAgentCursorAhead = errors.New("agent cursor is ahead of control plane; recovery required")

type AgentSyncRequest struct {
	AgentID           string
	BaseRevision      int64
	OverlayVersion    string
	RequireCheckpoint bool
	CheckInventory    bool
	Inventory         []*agentv1.ServiceCondition
}

// AgentSyncPlan renders independent channels from one immutable product prefix
// and one scoped live observation capture. Full workloads and decrypted env are
// rendered only for a checkpoint or assignments invalidated by journal changes.
type AgentSyncPlan struct {
	Cursor            int64
	AuthorityEpoch    uint64
	NodeConfigVersion string
	NodeConfig        *agentv1.AssignedNodeConfig
	OverlayVersion    string
	Images            []PullImage
	Checkpoint        *agentv1.DesiredNodeState
	Diffs             []*agentv1.AllocationDiff
}

type PullImage struct{ AllocationID, ServiceID, EnvironmentID, Image string }

func (d *Delivery) PlanAgentSync(ctx context.Context, request AgentSyncRequest) (*AgentSyncPlan, error) {
	if d == nil || d.live == nil {
		return nil, fmt.Errorf("live view is not available")
	}
	view, err := d.live.agentView(request.AgentID)
	if err != nil {
		return nil, err
	}
	cursor := view.product.Agents[request.AgentID].DesiredRevision
	if cursor < request.BaseRevision {
		return nil, ErrAgentCursorAhead
	}
	node, err := assignedNodeConfigForAgent(view.product, d.store.mesh, request.AgentID)
	if err != nil {
		return nil, err
	}
	overlays, images := view.overlayAndImages()
	plan := &AgentSyncPlan{Cursor: cursor, AuthorityEpoch: view.epoch, NodeConfig: node,
		NodeConfigVersion: reconciliation.HashNodeConfig(node), OverlayVersion: reconciliation.HashObservationOverlay(overlays), Images: images}
	checkpoint := func() (*AgentSyncPlan, error) {
		state, err := view.checkpoint(d.store.mesh)
		if err != nil {
			return nil, err
		}
		if err := d.applyServiceEnv(ctx, view.product, state); err != nil {
			return nil, err
		}
		state.NodeConfigVersion = plan.NodeConfigVersion
		plan.Checkpoint = state
		return plan, nil
	}
	if request.RequireCheckpoint || request.CheckInventory && (request.OverlayVersion != plan.OverlayVersion || !InventoriesMatch(request.Inventory, overlays)) ||
		cursor == request.BaseRevision && request.OverlayVersion != plan.OverlayVersion {
		return checkpoint()
	}
	if cursor == request.BaseRevision {
		return plan, nil
	}
	baseline := d.live.allocSync.baseline(request.AgentID, cursor)
	if !baseline.usable {
		return checkpoint()
	}
	if baseline.revision < cursor {
		// Host discovery and restart evidence can change independently of product
		// revisions. Compare each allocation's overlay so an added identity alone
		// never forces unchanged workloads through spec rendering and decryption.
		set := make(map[string]bool, len(baseline.dirty))
		for _, id := range baseline.dirty {
			set[id] = true
		}
		for _, svc := range overlays {
			id := svc.GetAllocationId()
			if previous, exists := baseline.services[id]; !exists || previous.Overlay != reconciliation.HashObservationOverlay([]*agentv1.DesiredService{svc}) {
				set[svc.GetAllocationId()] = true
			}
		}
		ids := slices.Collect(maps.Keys(set))
		slices.Sort(ids)
		services, volumes := baseline.services, baseline.volumes
		diff := storedDiff{Base: baseline.revision, Target: cursor}
		if len(ids) > 0 {
			currentServices, err := view.services(ids)
			if err != nil {
				return nil, err
			}
			partial := &agentv1.DesiredNodeState{Services: currentServices}
			if err := d.applyServiceEnv(ctx, view.product, partial); err != nil {
				return nil, err
			}
			services = maps.Clone(baseline.services)
			rendered := make(map[string]bool, len(currentServices))
			for _, svc := range currentServices {
				id, fp := svc.GetAllocationId(), serviceFingerprint(svc)
				rendered[id] = true
				if previous, exists := services[id]; !exists {
					diff.Starts = append(diff.Starts, svc)
				} else if previous != fp {
					diff.Updates = append(diff.Updates, svc)
				}
				services[id] = fp
			}
			for _, id := range ids {
				if !rendered[id] {
					if _, existed := services[id]; existed {
						diff.Stops = append(diff.Stops, id)
						delete(services, id)
					}
				}
			}
		}
		// The pinned volume set is small and rendered without spec parsing, so
		// every diff compares it in full.
		current := desiredVolumes(view.product, request.AgentID)
		volumes = make(map[string][32]byte, len(current))
		for _, volume := range current {
			id, fp := volume.GetVolumeId(), fingerprint(volume)
			volumes[id] = fp
			if previous, exists := baseline.volumes[id]; !exists || previous != fp {
				diff.VolumeStarts = append(diff.VolumeStarts, volume)
			}
		}
		for id := range baseline.volumes {
			if _, exists := volumes[id]; !exists {
				diff.VolumeStops = append(diff.VolumeStops, id)
			}
		}
		slices.Sort(diff.VolumeStops)
		diff.SizeBytes = diffPayloadSize(&diff)
		if !d.live.allocSync.acceptDiff(request.AgentID, baseline, cursor, services, volumes, diff) {
			return checkpoint()
		}
	}
	stored, target, ok := d.live.allocSync.diffsFrom(request.AgentID, request.BaseRevision)
	if !ok || target != cursor || len(stored) == 0 {
		return checkpoint()
	}
	for _, diff := range stored {
		plan.Diffs = append(plan.Diffs, diff.ToProto(request.AgentID))
	}
	return plan, nil
}

// AgentCheckpointSent establishes the only allocation baseline: the state
// actually delivered to an agent, including decrypted env and observation repair.
func (d *Delivery) AgentCheckpointSent(state *agentv1.DesiredNodeState) {
	if d == nil || d.live == nil || state == nil {
		return
	}
	l := d.live
	l.mu.Lock()
	defer l.mu.Unlock()
	id := state.GetAgentId()
	sessionMismatch := false
	if state.GetSessionId() != "" {
		session := l.sessions[id]
		sessionMismatch = session == nil || session.SessionID != state.GetSessionId()
	}
	// A write or ownership change can commit while a checkpoint is in flight.
	// Its invalidations may precede the creation of a baseline. Refuse that stale
	// baseline and establish the latest prefix on the next batch instead.
	if !l.serving || sessionMismatch || state.GetAuthorityEpoch() != l.authorityEpoch {
		return
	}
	if state.GetReconciliationCursor() != l.product.Agents[id].DesiredRevision {
		l.allocSync.discardThrough(id, state.GetReconciliationCursor())
		return
	}
	l.allocSync.rebase(id, state)
}

func (v *agentView) overlayAndImages() ([]*agentv1.DesiredService, []PullImage) {
	var overlays []*agentv1.DesiredService
	var images []PullImage
	for _, id := range v.product.AssignmentIDsForAgent(v.agentID) {
		a := v.product.Assignments[id]
		if a.RolloutState == AllocationRolloutLost {
			continue
		}
		image := v.product.Rollouts[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredRolloutGeneration)].ImageDigest
		if image == "" {
			continue
		}
		service := v.product.Services[a.ServiceID]
		internalHosts := v.internalHostsForEnvironment(service.EnvironmentID)
		obs := v.observations[liveObsKey{AllocationID: id, Generation: a.DesiredRolloutGeneration}]
		overlays = append(overlays, &agentv1.DesiredService{AllocationId: id, DesiredSpecRevision: a.DesiredSpecRevision,
			DesiredRolloutGeneration: a.DesiredRolloutGeneration, InternalHosts: internalHosts, RestartObservation: obs.Restart})
		images = append(images, PullImage{id, a.ServiceID, service.EnvironmentID, image})
	}
	return overlays, images
}
