package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func (d *Delivery) DesiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	if d == nil || d.live == nil {
		return nil, fmt.Errorf("live view is not available")
	}
	state, err := d.live.DesiredStateForAgent(agentID, d.store.mesh)
	if err != nil {
		return nil, err
	}
	// Sealed values decrypt here, on the control plane, for exactly this
	// agent's assignments. Agents receive runtime plaintext and never keys.
	if err := d.resolveSealedEnv(ctx, state); err != nil {
		return nil, err
	}
	// 2.10: credentials travel in PullCredentialSet, never in allocation
	// checkpoints or diffs. Node config carries its independent version.
	for _, svc := range state.GetServices() {
		svc.RegistryUsername = ""
		svc.RegistryPassword = ""
	}
	state.NodeConfigVersion = HashNodeConfig(state.GetNodeConfig())
	if d.allocSync != nil {
		d.allocSync.recordCurrent(agentID, state)
	}
	return state, nil
}

// AllocationDiffsFrom returns retained incremental diffs from base to the
// latest recorded revision for agentID. ok=false means send a checkpoint.
func (d *Delivery) AllocationDiffsFrom(agentID string, base int64) (diffs []*agentv1.AllocationDiff, target int64, ok bool) {
	if d == nil || d.allocSync == nil {
		return nil, 0, false
	}
	stored, target, ok := d.allocSync.diffsFrom(agentID, base)
	if !ok {
		return nil, target, false
	}
	for _, s := range stored {
		diffs = append(diffs, s.ToProto(agentID))
	}
	return diffs, target, true
}

func desiredVolumes(live journal.DurableState, agentID string) ([]*agentv1.DesiredVolume, error) {
	wanted := make(map[string]bool)
	for _, a := range live.Assignments {
		if a.AgentID != agentID {
			continue
		}
		service := live.Services[a.ServiceID]
		revision := live.Revisions[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredSpecRevision)]
		spec, err := LoadServiceSpec(revision.SpecJSON)
		if err != nil {
			return nil, err
		}
		if name := ServiceVolumeName(spec); name != "" {
			wanted[volumeKey(service.EnvironmentID, name)] = true
		}
	}
	var volumes []journal.Volume
	for _, v := range live.Volumes {
		if wanted[volumeKey(v.EnvironmentID, v.Name)] {
			volumes = append(volumes, v)
		}
	}
	slices.SortFunc(volumes, func(a, b journal.Volume) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	var out []*agentv1.DesiredVolume
	for _, v := range volumes {
		out = append(out, &agentv1.DesiredVolume{VolumeId: v.ID, EnvironmentId: v.EnvironmentID, Name: v.Name, SizeBytes: v.SizeBytes})
	}
	return out, nil
}

func workloadIdentities(live journal.DurableState, indexes liveIndexes, agentID string) ([]*agentv1.WorkloadIdentity, error) {
	var assignments []journal.Assignment
	for _, environmentID := range indexes.environmentsByAgent[agentID] {
		for _, serviceID := range indexes.servicesByEnvironment[environmentID] {
			for _, assignmentID := range indexes.assignmentsByService[serviceID] {
				a := live.Assignments[assignmentID]
				if a.RolloutState != AllocationRolloutLost {
					assignments = append(assignments, a)
				}
			}
		}
	}
	slices.SortFunc(assignments, func(a, b journal.Assignment) int {
		if n := live.Services[a.ServiceID].CreatedAt.Compare(live.Services[b.ServiceID].CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	var identities []*agentv1.WorkloadIdentity
	for _, a := range assignments {
		service := live.Services[a.ServiceID]
		environment := live.Environments[service.EnvironmentID]
		agent := live.Agents[a.AgentID]
		if environment.NetworkIdentity <= 0 || environment.NetworkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", service.EnvironmentID, environment.NetworkIdentity)
		}
		identities = append(identities, &agentv1.WorkloadIdentity{WorkloadIpv4: a.AllocationIPv4, WorkloadIpv6: a.AllocationIPv6, EnvironmentId: service.EnvironmentID, NetworkIdentity: uint32(environment.NetworkIdentity), HostAgentId: a.AgentID, HostIpv6: agent.AdvertiseAddr})
	}
	return identities, nil
}
