package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

const AgentAuthorityEpoch uint64 = 1

func (d *Delivery) DesiredStateForAgent(ctx context.Context, agentID string) (*agentv1.DesiredNodeState, error) {
	s := d.store
	revision, err := s.currentDesiredRevisionForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state := &agentv1.DesiredNodeState{
		AgentId:              agentID,
		ReconciliationCursor: revision,
		AuthorityEpoch:       AgentAuthorityEpoch,
		GeneratedAt:          ts(time.Now().UTC()),
	}
	vols, err := d.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.Volumes = vols
	services, err := d.listDesiredServices(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.Services = services
	nodeConfig, err := d.assignedNodeConfigForAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	state.NodeConfig = nodeConfig
	return state, nil
}

func (d *Delivery) listDesiredVolumes(ctx context.Context, agentID string) ([]*agentv1.DesiredVolume, error) {
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT DISTINCT v.id, v.environment_id, v.name, v.size_bytes, v.created_at
		   FROM volumes v
		   JOIN services s ON s.environment_id = v.environment_id
		   JOIN allocations a ON a.service_id = s.id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE a.agent_id = $1 AND COALESCE(r.spec_json->'runtime'->>'volumeName', '') = v.name
		  ORDER BY v.created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentv1.DesiredVolume
	for rows.Next() {
		vol := &agentv1.DesiredVolume{}
		var createdAt time.Time
		if err := rows.Scan(&vol.VolumeId, &vol.EnvironmentId, &vol.Name, &vol.SizeBytes, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, vol)
	}
	return out, rows.Err()
}

func (d *Delivery) listDesiredServices(ctx context.Context, agentID string) ([]*agentv1.DesiredService, error) {
	s := d.store
	volumes, err := d.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetEnvironmentId(), vol.GetName())] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.environment_id, s.name, a.deployment_id, a.desired_spec_revision, a.desired_rollout_generation,
		        ro.image_digest, r.spec_json, e.network_identity, e.name, p.id, p.name,
		        a.restart_observation_json, a.operator_restart_nonce, a.rollout_state, a.intent, a.drain_deadline,
		        a.allocation_ipv4, a.allocation_ipv6
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = a.desired_spec_revision
		   JOIN service_rollouts ro ON ro.service_id = s.id AND ro.rollout_generation = a.desired_rollout_generation
		  WHERE a.agent_id = $1
		    AND ro.image_digest <> ''
		  ORDER BY s.created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*agentv1.DesiredService
	for rows.Next() {
		svc := &agentv1.DesiredService{}
		var resolvedImage string
		var rawSpec []byte
		var networkIdentity int64
		var environmentName, projectID, projectName string
		var restartRaw []byte
		var rolloutState string
		var intent string
		var drainDeadline sql.NullTime
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.EnvironmentId, &svc.Name, &svc.DeploymentId, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &resolvedImage, &rawSpec, &networkIdentity, &environmentName, &projectID, &projectName, &restartRaw, &svc.OperatorRestartNonce, &rolloutState, &intent, &drainDeadline, &svc.PrivateIpv4, &svc.PrivateIpv6); err != nil {
			return nil, err
		}
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
		targetPorts, err := s.domainTargetPortsForService(ctx, svc.ServiceId)
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
		svc.InternalHosts, err = d.internalHostsForEnvironment(ctx, svc.EnvironmentId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (d *Delivery) internalHostsForEnvironment(ctx context.Context, environmentID string) ([]*agentv1.InternalHost, error) {
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT s.id, s.name, a.allocation_ipv4, a.allocation_ipv6,
		        a.healthy_ipv4_ports, a.healthy_ipv6_ports
		   FROM services s
		   JOIN allocations a ON a.service_id = s.id
		  WHERE s.environment_id = $1
		    AND a.healthy = TRUE
		    AND a.rollout_state = 'serving'
		    AND a.applied_spec_revision >= a.desired_spec_revision
		    AND a.applied_rollout_generation >= a.desired_rollout_generation
		  ORDER BY s.created_at ASC, a.id ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*agentv1.InternalHost
	for rows.Next() {
		var serviceID, name, ipv4, ipv6 string
		var healthyIPv4Ports, healthyIPv6Ports []int32
		if err := rows.Scan(&serviceID, &name, &ipv4, &ipv6, (*jsonInt32Slice)(&healthyIPv4Ports), (*jsonInt32Slice)(&healthyIPv6Ports)); err != nil {
			return nil, err
		}
		if len(healthyIPv4Ports) == 0 {
			ipv4 = ""
		}
		if len(healthyIPv6Ports) == 0 {
			ipv6 = ""
		}
		hosts = append(hosts, &agentv1.InternalHost{
			Hostname: InternalServiceHostname(name, serviceID),
			Ipv4:     ipv4,
			Ipv6:     ipv6,
		})
	}
	return hosts, rows.Err()
}

func (d *Delivery) assignedNodeConfigForAgent(ctx context.Context, agentID string) (*agentv1.AssignedNodeConfig, error) {
	s := d.store
	agent, err := s.agentByID(ctx, agentID)
	if err != nil {
		return nil, err
	}
	agents, err := s.listAgents(ctx)
	if err != nil {
		return nil, err
	}

	assigned := &agentv1.AssignedNodeConfig{
		WorkloadIpv4Subnet:     agent.WorkloadIPv4Subnet,
		WorkloadIpv4Pool:       s.mesh.WorkloadIPv4PoolCIDR,
		WorkloadIpv6Subnet:     agent.WorkloadIPv6Subnet,
		WorkloadIpv6Pool:       s.mesh.WorkloadPoolCIDR,
		WireguardInterfaceName: s.mesh.InterfaceName,
		WireguardAddresses:     []string{agent.WireGuardIPv6},
		WireguardListenPort:    int32(agent.WireGuardListenPort),
	}
	assigned.WorkloadIdentities, err = d.listWorkloadIdentities(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range agents {
		if peer.ID == agentID || peer.LifecycleState == AgentStateRetired || peer.CredentialRevokedAt.Valid || peer.WireGuardPublicKey == "" || peer.WireGuardListenPort <= 0 || peer.WorkloadIPv4Subnet == "" || peer.WorkloadIPv6Subnet == "" {
			continue
		}
		endpoint, err := endpointForAgent(peer.AdvertiseAddr, peer.WireGuardListenPort)
		if err != nil {
			return nil, err
		}
		assigned.Peers = append(assigned.Peers, &agentv1.WireGuardPeer{
			AgentId:                    peer.ID,
			Name:                       peer.Name,
			PublicKey:                  peer.WireGuardPublicKey,
			Endpoint:                   endpoint,
			AllowedIps:                 []string{peer.WorkloadIPv4Subnet, peer.WorkloadIPv6Subnet},
			PersistentKeepaliveSeconds: int32(s.mesh.PersistentKeepaliveSeconds),
		})
	}
	return assigned, nil
}

func (d *Delivery) listWorkloadIdentities(ctx context.Context) ([]*agentv1.WorkloadIdentity, error) {
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT a.allocation_ipv4, a.allocation_ipv6, s.environment_id, e.network_identity, a.agent_id, ag.advertise_addr
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN agents ag ON ag.id = a.agent_id
		  WHERE a.rollout_state <> $1
		  ORDER BY s.created_at ASC, a.id ASC`,
		AllocationRolloutLost,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var identities []*agentv1.WorkloadIdentity
	for rows.Next() {
		var (
			workloadIPv4    string
			workloadIPv6    string
			environmentID   string
			networkIdentity int64
			hostAgentID     string
			hostIPv6        string
		)
		if err := rows.Scan(&workloadIPv4, &workloadIPv6, &environmentID, &networkIdentity, &hostAgentID, &hostIPv6); err != nil {
			return nil, err
		}
		if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", environmentID, networkIdentity)
		}
		identities = append(identities, &agentv1.WorkloadIdentity{
			WorkloadIpv4:    workloadIPv4,
			WorkloadIpv6:    workloadIPv6,
			EnvironmentId:   environmentID,
			NetworkIdentity: uint32(networkIdentity),
			HostAgentId:     hostAgentID,
			HostIpv6:        hostIPv6,
		})
	}
	return identities, rows.Err()
}
