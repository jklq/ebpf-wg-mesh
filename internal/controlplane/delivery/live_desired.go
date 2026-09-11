package delivery

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func (l *Live) DesiredStateForAgent(agentID string, mesh config.ControlPlaneMeshConfig) (*agentv1.DesiredNodeState, error) {
	if l == nil {
		return nil, sql.ErrNoRows
	}
	l.mu.Lock()
	durable := l.durable.Clone()
	epoch := l.authorityEpoch
	now := l.now().UTC()
	ttl := l.ttl
	sessions := make(map[string]AgentSession, len(l.sessions))
	for id, session := range l.sessions {
		if session != nil {
			sessions[id] = *session
		}
	}
	observations := make(map[liveObsKey]AllocationObservation, len(l.observations))
	for key, obs := range l.observations {
		observations[key] = obs
	}
	indexes := cloneLiveIndexes(l.indexes)
	l.mu.Unlock()
	return buildDesiredState(agentID, durable, sessions, observations, indexes, now, ttl, epoch, mesh)
}

func cloneLiveIndexes(idx liveIndexes) liveIndexes {
	out := newLiveIndexes()
	for k, v := range idx.assignmentsByAgent {
		out.assignmentsByAgent[k] = append([]string(nil), v...)
	}
	for k, v := range idx.assignmentsByService {
		out.assignmentsByService[k] = append([]string(nil), v...)
	}
	for k, v := range idx.domainsByService {
		out.domainsByService[k] = append([]string(nil), v...)
	}
	for k, v := range idx.servicesByEnvironment {
		out.servicesByEnvironment[k] = append([]string(nil), v...)
	}
	return out
}

func buildDesiredState(agentID string, durable journal.DurableState, sessions map[string]AgentSession, observations map[liveObsKey]AllocationObservation, indexes liveIndexes, now time.Time, ttl time.Duration, epoch uint64, mesh config.ControlPlaneMeshConfig) (*agentv1.DesiredNodeState, error) {
	agent, exists := durable.Agents[agentID]
	if !exists {
		return nil, sql.ErrNoRows
	}
	candidate := &agentv1.DesiredNodeState{
		AgentId: agentID, ReconciliationCursor: agent.DesiredRevision,
		AuthorityEpoch: epoch, Scope: agentv1.SnapshotScope_SNAPSHOT_SCOPE_AGENT, GeneratedAt: ts(now),
	}
	var err error
	candidate.Volumes, err = desiredVolumes(durable, agentID)
	if err != nil {
		return nil, err
	}
	candidate.Services, err = listDesiredServices(durable, sessions, observations, indexes, agentID, now, ttl)
	if err != nil {
		return nil, err
	}
	candidate.NodeConfig, err = assignedNodeConfigForAgent(durable, mesh, agentID)
	if err != nil {
		return nil, err
	}
	candidate.Complete = true
	return candidate, nil
}

func listDesiredServices(durable journal.DurableState, sessions map[string]AgentSession, observations map[liveObsKey]AllocationObservation, indexes liveIndexes, agentID string, now time.Time, ttl time.Duration) ([]*agentv1.DesiredService, error) {
	volumes, err := desiredVolumes(durable, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetEnvironmentId(), vol.GetName())] = vol.GetVolumeId()
	}

	samples := make(map[string][]byte)
	for _, id := range indexes.assignmentsByAgent[agentID] {
		a := durable.Assignments[id]
		obs, ok := observations[liveObsKey{AllocationID: id, Generation: a.DesiredRolloutGeneration}]
		if !ok || obs.Restart == nil {
			continue
		}
		raw, err := encodeRestartObservation(obs.Restart)
		if err != nil {
			return nil, err
		}
		samples[id] = raw
	}

	type serviceRow struct {
		svc                                     *agentv1.DesiredService
		resolvedImage                           string
		rawSpec                                 []byte
		networkIdentity                         int64
		environmentName, projectID, projectName string
		restartRaw                              []byte
		rolloutState, intent                    string
		drainDeadline                           sql.NullTime
	}
	var pending []serviceRow
	var assignments []journal.Assignment
	for _, id := range indexes.assignmentsByAgent[agentID] {
		assignments = append(assignments, durable.Assignments[id])
	}
	slices.SortFunc(assignments, func(a, b journal.Assignment) int {
		if n := durable.Services[a.ServiceID].CreatedAt.Compare(durable.Services[b.ServiceID].CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, a := range assignments {
		service := durable.Services[a.ServiceID]
		environment := durable.Environments[service.EnvironmentID]
		project := durable.Projects[environment.ProjectID]
		rollout := durable.Rollouts[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredRolloutGeneration)]
		if rollout.ImageDigest == "" {
			continue
		}
		revision := durable.Revisions[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredSpecRevision)]
		svc := &agentv1.DesiredService{AllocationId: a.ID, ServiceId: a.ServiceID, EnvironmentId: service.EnvironmentID, Name: service.Name, DeploymentId: a.DeploymentID,
			DesiredSpecRevision: a.DesiredSpecRevision, DesiredRolloutGeneration: a.DesiredRolloutGeneration, OperatorRestartNonce: a.OperatorRestartNonce, PrivateIpv4: a.AllocationIPv4, PrivateIpv6: a.AllocationIPv6}
		deadline := sql.NullTime{}
		if a.DrainDeadline != nil {
			deadline = sql.NullTime{Time: *a.DrainDeadline, Valid: true}
		}
		restartRaw := samples[a.ID]
		if len(restartRaw) == 0 {
			restartRaw = []byte("{}")
		}
		pending = append(pending, serviceRow{svc, rollout.ImageDigest, revision.SpecJSON, environment.NetworkIdentity, environment.Name, project.ID, project.Name, restartRaw, a.RolloutState, a.Intent, deadline})
	}
	var out []*agentv1.DesiredService
	for _, row := range pending {
		svc, resolvedImage, rawSpec, networkIdentity := row.svc, row.resolvedImage, row.rawSpec, row.networkIdentity
		environmentName, projectID, projectName := row.environmentName, row.projectID, row.projectName
		restartRaw, rolloutState, intent, drainDeadline := row.restartRaw, row.rolloutState, row.intent, row.drainDeadline
		if rolloutState == AllocationRolloutLost {
			continue
		}
		svc.Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_RUN
		if intent == allocationIntentDrain {
			svc.Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN
			if drainDeadline.Valid {
				svc.DrainDeadline = ts(drainDeadline.Time)
			}
		}
		if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", svc.EnvironmentId, networkIdentity)
		}
		svc.NetworkIdentity = uint32(networkIdentity)
		spec, err := LoadServiceSpec(rawSpec)
		if err != nil {
			return nil, err
		}
		obs, err := decodeRestartObservation(restartRaw)
		if err != nil {
			return nil, err
		}
		svc.RestartObservation = obs
		svc.Spec = resolvedDesiredServiceSpec(spec, resolvedImage, domainTargetPortsFromDurable(durable, indexes.domainsByService[svc.ServiceId], svc.ServiceId))
		if svc.Spec.Runtime.Env == nil {
			svc.Spec.Runtime.Env = make(map[string]string)
		}
		platformEnv := map[string]string{
			"PLATFORM_PROJECT_ID": projectID, "PLATFORM_PROJECT_NAME": projectName,
			"PLATFORM_ENVIRONMENT_ID": svc.EnvironmentId, "PLATFORM_ENVIRONMENT_NAME": environmentName,
			"PLATFORM_SERVICE_ID": svc.ServiceId, "PLATFORM_SERVICE_NAME": svc.Name,
			"PLATFORM_DEPLOYMENT_ID": svc.GetDeploymentId(),
		}
		for key, value := range platformEnv {
			svc.Spec.Runtime.Env[key] = value
		}
		if volumeName := ServiceVolumeName(spec); volumeName != "" {
			svc.VolumeId = volumeIDs[volumeKey(svc.EnvironmentId, volumeName)]
		}
		svc.InternalHostname = InternalServiceHostname(svc.Name, svc.ServiceId)
		svc.InternalHosts, err = internalHostsForEnvironment(durable, sessions, observations, indexes, now, ttl, svc.EnvironmentId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, nil
}

func internalHostsForEnvironment(durable journal.DurableState, sessions map[string]AgentSession, observations map[liveObsKey]AllocationObservation, indexes liveIndexes, now time.Time, ttl time.Duration, environmentID string) ([]*agentv1.InternalHost, error) {
	type hostRow struct {
		created              time.Time
		id, name, ipv4, ipv6 string
	}
	var rows []hostRow
	for _, serviceID := range indexes.servicesByEnvironment[environmentID] {
		service := durable.Services[serviceID]
		for _, assignmentID := range indexes.assignmentsByService[serviceID] {
			assignment := durable.Assignments[assignmentID]
			rec := allocationRecordFromAssignment(durable, assignment)
			session, hasSession := sessions[rec.AgentID]
			obs, hasObs := observations[liveObsKey{AllocationID: rec.ID, Generation: rec.DesiredRolloutGeneration}]
			rec = overlayAllocation(rec, session, hasSession, obs, hasObs, now, ttl)
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
	return hosts, nil
}

func assignedNodeConfigForAgent(durable journal.DurableState, mesh config.ControlPlaneMeshConfig, agentID string) (*agentv1.AssignedNodeConfig, error) {
	agent, ok := durable.Agents[agentID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	var agents []journal.AgentRegistration
	for _, peer := range durable.Agents {
		agents = append(agents, peer)
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
	identities, err := workloadIdentities(durable)
	assigned.WorkloadIdentities = identities
	if err != nil {
		return nil, err
	}
	for _, peer := range agents {
		administration := durable.Administration[peer.ID]
		if peer.ID == agentID || administration.LifecycleState == string(AgentStateRetired) || administration.CredentialRevokedAt != nil || peer.WireguardPublicKey == "" || peer.WireguardEndpoint == "" || peer.WorkloadIPv4Subnet == "" || peer.WorkloadIPv6Subnet == "" {
			continue
		}
		assigned.Peers = append(assigned.Peers, &agentv1.WireGuardPeer{
			AgentId:                    peer.ID,
			Name:                       peer.Name,
			PublicKey:                  peer.WireguardPublicKey,
			Endpoint:                   peer.WireguardEndpoint,
			AllowedIps:                 []string{peer.WorkloadIPv4Subnet, peer.WorkloadIPv6Subnet},
			PersistentKeepaliveSeconds: int32(mesh.PersistentKeepaliveSeconds),
		})
	}
	return assigned, nil
}
