package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/protobuf/proto"
)

func (d *Delivery) DesiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	if d == nil || d.live == nil {
		return nil, fmt.Errorf("live view is not available")
	}
	view, err := d.live.agentView(agentID)
	if err != nil {
		return nil, err
	}
	state, err := view.checkpoint(d.store.mesh)
	if err != nil {
		return nil, err
	}
	// Env decrypts here for this agent's assignments only; agents never get keys.
	if err := d.applyServiceEnv(ctx, view.product, state); err != nil {
		return nil, err
	}
	// Credentials travel in PullCredentialSet only; checkpoints and diffs must never carry them.
	for _, svc := range state.GetServices() {
		svc.RegistryUsername = ""
		svc.RegistryPassword = ""
	}
	state.NodeConfigVersion = reconciliation.HashNodeConfig(state.GetNodeConfig())
	return state, nil
}

// desiredVolumes is every live volume pinned to the agent, attached or not,
// plus explicit destroy instructions. A tombstoned volume is absent during its
// deletion grace; the agent retains its data until a destruction arrives.
func desiredVolumes(product *journal.Projection, agentID string) []*agentv1.DesiredVolume {
	var out []*agentv1.DesiredVolume
	for _, id := range product.VolumeIDsForAgent(agentID) {
		v := product.Volumes[id]
		out = append(out, &agentv1.DesiredVolume{VolumeId: v.ID, EnvironmentId: v.EnvironmentID, Name: v.Name, SizeBytes: v.SizeBytes})
	}
	for _, id := range product.DestructionIDsForAgent(agentID) {
		if _, live := product.Volumes[id]; live {
			continue
		}
		out = append(out, &agentv1.DesiredVolume{VolumeId: id, Destroy: true})
	}
	slices.SortFunc(out, func(a, b *agentv1.DesiredVolume) int { return strings.Compare(a.GetVolumeId(), b.GetVolumeId()) })
	return out
}

// environmentVolumeID resolves a service's volume name within its environment.
func environmentVolumeID(product *journal.Projection, environmentID, name string) string {
	for _, id := range product.VolumeIDsForEnvironment(environmentID) {
		if product.Volumes[id].Name == name {
			return id
		}
	}
	return ""
}

func workloadIdentities(product *journal.Projection, agentID string) ([]*agentv1.WorkloadIdentity, error) {
	live := product.DurableState
	var assignments []journal.Assignment
	for _, environmentID := range product.EnvironmentIDsForAgent(agentID) {
		for _, serviceID := range product.ServiceIDsForEnvironment(environmentID) {
			for _, assignmentID := range product.AssignmentIDsForService(serviceID) {
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

type agentView struct {
	agentID            string
	product            *journal.Projection
	sessions           map[string]AgentSession
	observations       map[liveObsKey]AllocationObservation
	now                time.Time
	ttl                time.Duration
	epoch              uint64
	hostsByEnvironment map[string][]*agentv1.InternalHost
}

func (l *Live) DesiredStateForAgent(agentID string, mesh config.ControlPlaneMeshConfig) (*agentv1.DesiredNodeState, error) {
	view, err := l.agentView(agentID)
	if err != nil {
		return nil, err
	}
	return view.checkpoint(mesh)
}

func (l *Live) agentView(agentID string) (*agentView, error) {
	if l == nil {
		return nil, sql.ErrNoRows
	}
	l.mu.Lock()
	product := l.product
	epoch, now, ttl := l.authorityEpoch, l.now().UTC(), l.ttl
	// Only observations and presence in the agent's private network scopes
	// affect its rendering. Durable rows and indexes are already immutable.
	assignmentIDs := make(map[string]bool)
	for _, id := range product.AssignmentIDsForAgent(agentID) {
		assignmentIDs[id] = true
	}
	for _, env := range product.EnvironmentIDsForAgent(agentID) {
		for _, service := range product.ServiceIDsForEnvironment(env) {
			for _, id := range product.AssignmentIDsForService(service) {
				assignmentIDs[id] = true
			}
		}
	}
	sessions := make(map[string]AgentSession)
	observations := make(map[liveObsKey]AllocationObservation)
	for id := range assignmentIDs {
		a := product.Assignments[id]
		if session := l.sessions[a.AgentID]; session != nil {
			sessions[a.AgentID] = *session
		}
		key := liveObsKey{AllocationID: id, Generation: a.DesiredRolloutGeneration}
		if obs, ok := l.observations[key]; ok {
			observations[key] = obs
		}
	}
	l.mu.Unlock()
	if _, exists := product.Agents[agentID]; !exists {
		return nil, sql.ErrNoRows
	}
	return &agentView{agentID: agentID, product: product, sessions: sessions, observations: observations, now: now, ttl: ttl, epoch: epoch}, nil
}

func (v *agentView) checkpoint(mesh config.ControlPlaneMeshConfig) (*agentv1.DesiredNodeState, error) {
	candidate := &agentv1.DesiredNodeState{AgentId: v.agentID, ReconciliationCursor: v.product.Agents[v.agentID].DesiredRevision,
		AuthorityEpoch: v.epoch, Scope: agentv1.SnapshotScope_SNAPSHOT_SCOPE_AGENT, GeneratedAt: ts(v.now), Complete: true}
	var err error
	candidate.Volumes = desiredVolumes(v.product, v.agentID)
	candidate.Services, err = v.services(v.product.AssignmentIDsForAgent(v.agentID))
	if err != nil {
		return nil, err
	}
	candidate.NodeConfig, err = assignedNodeConfigForAgent(v.product, mesh, v.agentID)
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func (v *agentView) services(assignmentIDs []string) ([]*agentv1.DesiredService, error) {
	product := v.product
	var assignments []journal.Assignment
	for _, id := range assignmentIDs {
		a, exists := product.Assignments[id]
		if exists && a.AgentID == v.agentID && a.RolloutState != AllocationRolloutLost {
			assignments = append(assignments, a)
		}
	}
	slices.SortFunc(assignments, func(a, b journal.Assignment) int {
		if n := product.Services[a.ServiceID].CreatedAt.Compare(product.Services[b.ServiceID].CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	var out []*agentv1.DesiredService
	for _, a := range assignments {
		service := product.Services[a.ServiceID]
		environment := product.Environments[service.EnvironmentID]
		project := product.Projects[environment.ProjectID]
		rollout := product.Rollouts[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredRolloutGeneration)]
		if rollout.ImageDigest == "" {
			continue
		}
		if environment.NetworkIdentity <= 0 || environment.NetworkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", service.EnvironmentID, environment.NetworkIdentity)
		}
		revision := product.Revisions[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredSpecRevision)]
		spec, err := LoadServiceSpec(revision.SpecJSON)
		if err != nil {
			return nil, err
		}
		svc := &agentv1.DesiredService{AllocationId: a.ID, ServiceId: a.ServiceID, EnvironmentId: service.EnvironmentID, Name: service.Name, DeploymentId: a.DeploymentID, CreatedAt: ts(a.CreatedAt),
			DesiredSpecRevision: a.DesiredSpecRevision, DesiredRolloutGeneration: a.DesiredRolloutGeneration, OperatorRestartNonce: a.OperatorRestartNonce,
			PrivateIpv4: a.AllocationIPv4, PrivateIpv6: a.AllocationIPv6, NetworkIdentity: uint32(environment.NetworkIdentity), Intent: agentv1.AllocationIntent_ALLOCATION_INTENT_RUN}
		if a.Intent == allocationIntentDrain {
			svc.Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN
			if a.DrainDeadline != nil {
				svc.DrainDeadline = ts(*a.DrainDeadline)
			}
		}
		if obs := v.observations[liveObsKey{AllocationID: a.ID, Generation: a.DesiredRolloutGeneration}]; obs.Restart != nil {
			svc.RestartObservation = proto.Clone(obs.Restart).(*platformv1.RestartObservation)
		}
		svc.Spec = resolvedDesiredServiceSpec(spec, rollout.ImageDigest, domainTargetPortsFromDurable(product.DurableState, product.DomainHostnamesForService(a.ServiceID), a.ServiceID))
		if svc.Spec.Runtime.Env == nil {
			svc.Spec.Runtime.Env = make(map[string]string)
		}
		for key, value := range map[string]string{
			"PLATFORM_PROJECT_ID": project.ID, "PLATFORM_PROJECT_NAME": project.Name, "PLATFORM_ENVIRONMENT_ID": environment.ID, "PLATFORM_ENVIRONMENT_NAME": environment.Name,
			"PLATFORM_SERVICE_ID": service.ID, "PLATFORM_SERVICE_NAME": service.Name, "PLATFORM_DEPLOYMENT_ID": a.DeploymentID,
		} {
			svc.Spec.Runtime.Env[key] = value
		}
		// The agent refuses to start a mount that is not pinned to it, so a
		// placement bug surfaces as an error instead of an empty volume.
		if name := ServiceVolumeName(spec); name != "" {
			svc.VolumeId = environmentVolumeID(product, service.EnvironmentID, name)
		}
		svc.InternalHostname = InternalServiceHostname(service.Name, a.ServiceID)
		svc.InternalHosts = v.internalHostsForEnvironment(service.EnvironmentID)
		out = append(out, svc)
	}
	return out, nil
}

func (v *agentView) internalHostsForEnvironment(environmentID string) []*agentv1.InternalHost {
	if hosts, known := v.hostsByEnvironment[environmentID]; known {
		return hosts
	}
	product := v.product
	durable := product.DurableState
	type hostRow struct {
		created              time.Time
		id, name, ipv4, ipv6 string
	}
	var rows []hostRow
	for _, serviceID := range product.ServiceIDsForEnvironment(environmentID) {
		service := durable.Services[serviceID]
		for _, assignmentID := range product.AssignmentIDsForService(serviceID) {
			assignment := durable.Assignments[assignmentID]
			rec := allocationRecordFromAssignment(durable, assignment)
			session, hasSession := v.sessions[rec.AgentID]
			obs, hasObs := v.observations[liveObsKey{AllocationID: rec.ID, Generation: rec.DesiredRolloutGeneration}]
			rec = overlayAllocation(rec, session, hasSession, obs, hasObs, v.now, v.ttl)
			if !rec.Healthy || rec.RolloutState != AllocationRolloutServing ||
				rec.AppliedSpecRevision < rec.DesiredSpecRevision || rec.AppliedRolloutGeneration < rec.DesiredRolloutGeneration {
				continue
			}
			ipv4, ipv6 := rec.AllocationIPv4, rec.AllocationIPv6
			if len(rec.HealthyIPv4Ports) == 0 {
				ipv4 = ""
			}
			if len(rec.HealthyIPv6Ports) == 0 {
				ipv6 = ""
			}
			rows = append(rows, hostRow{service.CreatedAt, service.ID, service.Name, ipv4, ipv6})
		}
	}
	slices.SortFunc(rows, func(a, b hostRow) int {
		if n := a.created.Compare(b.created); n != 0 {
			return n
		}
		return strings.Compare(a.id, b.id)
	})
	var hosts []*agentv1.InternalHost
	for _, row := range rows {
		hosts = append(hosts, &agentv1.InternalHost{
			Hostname: InternalServiceHostname(row.name, row.id),
			Ipv4:     row.ipv4,
			Ipv6:     row.ipv6,
		})
	}
	if v.hostsByEnvironment == nil {
		v.hostsByEnvironment = make(map[string][]*agentv1.InternalHost)
	}
	v.hostsByEnvironment[environmentID] = hosts
	return hosts
}

func assignedNodeConfigForAgent(product *journal.Projection, mesh config.ControlPlaneMeshConfig, agentID string) (*agentv1.AssignedNodeConfig, error) {
	durable := product.DurableState
	agent, ok := durable.Agents[agentID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	var agents []journal.AgentRegistration
	seen := make(map[string]bool)
	for _, environmentID := range product.EnvironmentIDsForAgent(agentID) {
		for _, peerID := range product.AgentIDsForEnvironment(environmentID) {
			if !seen[peerID] {
				if peer, ok := durable.Agents[peerID]; ok {
					agents = append(agents, peer)
				}
				seen[peerID] = true
			}
		}
	}
	slices.SortFunc(agents, func(a, b journal.AgentRegistration) int { return strings.Compare(a.ID, b.ID) })
	assigned := &agentv1.AssignedNodeConfig{
		WorkloadIpv4Subnet:     agent.WorkloadIPv4Subnet,
		WorkloadIpv4Pool:       mesh.WorkloadIPv4PoolCIDR,
		WorkloadIpv6Subnet:     agent.WorkloadIPv6Subnet,
		WorkloadIpv6Pool:       mesh.WorkloadPoolCIDR,
		WireguardInterfaceName: mesh.InterfaceName,
		WireguardAddresses:     []string{agent.WireguardIPv6},
		WireguardListenPort:    int32(agent.WireguardListenPort),
	}
	identities, err := workloadIdentities(product, agentID)
	assigned.WorkloadIdentities = identities
	if err != nil {
		return nil, err
	}
	for _, peer := range agents {
		if peer.ID == agentID {
			continue
		}
		if rendered := renderWireGuardPeer(peer, durable.Administration[peer.ID], int32(mesh.PersistentKeepaliveSeconds)); rendered != nil {
			assigned.Peers = append(assigned.Peers, rendered)
		}
	}
	return assigned, nil
}

// renderWireGuardPeer defines both peer visibility and the fields whose changes
// invalidate agents sharing an environment with this peer.
func renderWireGuardPeer(peer journal.AgentRegistration, admin journal.AgentAdministration, keepalive int32) *agentv1.WireGuardPeer {
	if admin.LifecycleState == string(AgentStateRetired) || admin.CredentialRevokedAt != nil || peer.WireguardPublicKey == "" || peer.WireguardEndpoint == "" || peer.WorkloadIPv4Subnet == "" || peer.WorkloadIPv6Subnet == "" {
		return nil
	}
	return &agentv1.WireGuardPeer{AgentId: peer.ID, Name: peer.Name, PublicKey: peer.WireguardPublicKey, Endpoint: peer.WireguardEndpoint, AllowedIps: []string{peer.WorkloadIPv4Subnet, peer.WorkloadIPv6Subnet}, PersistentKeepaliveSeconds: keepalive}
}
