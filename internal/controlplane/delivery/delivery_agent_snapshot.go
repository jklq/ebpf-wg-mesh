package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func (d *Delivery) DesiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	var state *agentv1.DesiredNodeState
	err := d.store.readState(ctx, func(tx *sql.Tx, live journal.DurableState) error {
		agent, exists := live.Agents[agentID]
		if !exists {
			return sql.ErrNoRows
		}
		revision := agent.DesiredRevision
		var epoch uint64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch); err != nil {
			return err
		}
		candidate := &agentv1.DesiredNodeState{AgentId: agentID, ReconciliationCursor: revision,
			AuthorityEpoch: epoch, Scope: agentv1.SnapshotScope_SNAPSHOT_SCOPE_AGENT, GeneratedAt: ts(time.Now().UTC())}
		var err error
		candidate.Volumes, err = desiredVolumes(live, agentID)
		if err != nil {
			return err
		}
		candidate.Services, err = d.listDesiredServices(ctx, tx, live, agentID)
		if err != nil {
			return err
		}
		candidate.NodeConfig, err = d.assignedNodeConfigForAgent(live, agentID)
		if err != nil {
			return err
		}
		candidate.Complete = true
		state = candidate
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
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

func (d *Delivery) listDesiredServices(ctx context.Context, q ServiceQueryer, live journal.DurableState, agentID string) ([]*agentv1.DesiredService, error) {
	volumes, err := desiredVolumes(live, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetEnvironmentId(), vol.GetName())] = vol.GetVolumeId()
	}

	// Restart samples are transient. Assignment intent and addresses come only
	// from the applied committed prefix, never from an attempted SQL mutation.
	samples := make(map[string][]byte)
	for id, a := range live.Assignments {
		if a.AgentID != agentID {
			continue
		}
		obs, ok := d.store.live.Observation(id, a.DesiredRolloutGeneration)
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
	for _, a := range live.Assignments {
		if a.AgentID == agentID {
			assignments = append(assignments, a)
		}
	}
	slices.SortFunc(assignments, func(a, b journal.Assignment) int {
		if n := live.Services[a.ServiceID].CreatedAt.Compare(live.Services[b.ServiceID].CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, a := range assignments {
		service := live.Services[a.ServiceID]
		environment := live.Environments[service.EnvironmentID]
		project := live.Projects[environment.ProjectID]
		rollout := live.Rollouts[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredRolloutGeneration)]
		if rollout.ImageDigest == "" {
			continue
		}
		revision := live.Revisions[fmt.Sprintf("%s/%d", a.ServiceID, a.DesiredSpecRevision)]
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
		targetPorts, err := domainTargetPortsForServiceQuerier(ctx, q, svc.ServiceId)
		if err != nil {
			return nil, err
		}
		obs, err := decodeRestartObservation(restartRaw)
		if err != nil {
			return nil, err
		}
		svc.RestartObservation = obs
		svc.Spec = resolvedDesiredServiceSpec(spec, resolvedImage, targetPorts)
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
		svc.InternalHosts, err = d.internalHostsForEnvironment(live, svc.EnvironmentId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, nil
}

func (d *Delivery) internalHostsForEnvironment(durable journal.DurableState, environmentID string) ([]*agentv1.InternalHost, error) {
	type hostRow struct {
		created time.Time
		id, name, ipv4, ipv6 string
	}
	var rows []hostRow
	for _, service := range durable.Services {
		if service.EnvironmentID != environmentID {
			continue
		}
		for _, assignment := range durable.Assignments {
			if assignment.ServiceID != service.ID {
				continue
			}
			rec := d.store.overlayAllocation(AllocationRecord{
				ID: assignment.ID, ServiceID: service.ID, AgentID: assignment.AgentID,
				DesiredSpecRevision: assignment.DesiredSpecRevision, DesiredRolloutGeneration: assignment.DesiredRolloutGeneration,
				AllocationIPv4: assignment.AllocationIPv4, AllocationIPv6: assignment.AllocationIPv6,
				RolloutState: assignment.RolloutState, Message: assignment.IntentMessage,
			})
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

func (d *Delivery) assignedNodeConfigForAgent(live journal.DurableState, agentID string) (*agentv1.AssignedNodeConfig, error) {
	s := d.store
	agent, ok := live.Agents[agentID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	var agents []journal.AgentRegistration
	for _, peer := range live.Agents {
		agents = append(agents, peer)
	}
	slices.SortFunc(agents, func(a, b journal.AgentRegistration) int { return strings.Compare(a.ID, b.ID) })
	assigned := &agentv1.AssignedNodeConfig{
		WorkloadIpv4Subnet:     agent.WorkloadIPv4Subnet,
		WorkloadIpv4Pool:       s.mesh.WorkloadIPv4PoolCIDR,
		WorkloadIpv6Subnet:     agent.WorkloadIPv6Subnet,
		WorkloadIpv6Pool:       s.mesh.WorkloadPoolCIDR,
		WireguardInterfaceName: s.mesh.InterfaceName,
		WireguardAddresses:     []string{agent.WireguardIPv6},
		WireguardListenPort:    int32(agent.WireguardListenPort),
	}
	identities, err := workloadIdentities(live)
	assigned.WorkloadIdentities = identities
	if err != nil {
		return nil, err
	}
	for _, peer := range agents {
		administration := live.Administration[peer.ID]
		if peer.ID == agentID || administration.LifecycleState == string(AgentStateRetired) || administration.CredentialRevokedAt != nil || peer.WireguardPublicKey == "" || peer.WireguardListenPort <= 0 || peer.WorkloadIPv4Subnet == "" || peer.WorkloadIPv6Subnet == "" {
			continue
		}
		endpoint, err := endpointForAgent(peer.AdvertiseAddr, int(peer.WireguardListenPort))
		if err != nil {
			return nil, err
		}
		assigned.Peers = append(assigned.Peers, &agentv1.WireGuardPeer{
			AgentId:                    peer.ID,
			Name:                       peer.Name,
			PublicKey:                  peer.WireguardPublicKey,
			Endpoint:                   endpoint,
			AllowedIps:                 []string{peer.WorkloadIPv4Subnet, peer.WorkloadIPv6Subnet},
			PersistentKeepaliveSeconds: int32(s.mesh.PersistentKeepaliveSeconds),
		})
	}
	return assigned, nil
}

func workloadIdentities(live journal.DurableState) ([]*agentv1.WorkloadIdentity, error) {
	var assignments []journal.Assignment
	for _, a := range live.Assignments {
		if a.RolloutState != AllocationRolloutLost {
			assignments = append(assignments, a)
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
