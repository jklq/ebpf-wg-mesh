package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"
)

func (s *Store) chooseAgentForServiceTx(ctx context.Context, tx *sql.Tx, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, environmentID, volumeName); err != nil {
			return "", err
		}
	}
	return s.chooseAgentForPlacementQuerier(ctx, tx, spec)
}

func (s *Store) placementCandidatesQuerier(ctx context.Context, q serviceQueryer) ([]placementCandidate, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT a.id,
		        a.region,
		        a.zone,
		        a.failure_domain,
		        a.runtime_capabilities,
		        greatest(a.cpu_millis_capacity - a.reserved_cpu_millis, 0),
		        greatest(a.memory_mebibytes_capacity - a.reserved_memory_mebibytes, 0),
		        COALESCE(stats.service_count, 0),
		        COALESCE(stats.cpu_millis, 0),
		        COALESCE(stats.memory_mebibytes, 0)
		   FROM agents a
		   LEFT JOIN (
				SELECT a.agent_id,
		               COUNT(*) AS service_count,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
			  FROM services s
			  JOIN allocations a ON a.service_id = s.id
		          JOIN service_revisions r
		            ON r.service_id = s.id
		           AND r.spec_revision = s.current_spec_revision
			 GROUP BY a.agent_id
		   ) AS stats
		     ON stats.agent_id = a.id
		  WHERE a.last_seen_at > statement_timestamp() - INTERVAL '30 seconds' AND a.lifecycle_state = 'active'
		  ORDER BY COALESCE(stats.service_count, 0) ASC, a.id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []placementCandidate
	for rows.Next() {
		var candidate placementCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.Region,
			&candidate.Zone,
			&candidate.FailureDomain,
			(*jsonStringSlice)(&candidate.RuntimeCapabilities),
			&candidate.CPUMillisCapacity,
			&candidate.MemoryMebibytesCapcity,
			&candidate.ServiceCount,
			&candidate.UsedCPUMillis,
			&candidate.UsedMemoryMebibytes,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) requireVolumeQuerier(ctx context.Context, q serviceQueryer, environmentID, volumeName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM volumes WHERE environment_id = $1 AND name = $2`, environmentID, volumeName).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", errVolumeNotFound, volumeName)
	default:
		return err
	}
}

func (s *Store) listDesiredVolumes(ctx context.Context, agentID string) ([]*agentv1.DesiredVolume, error) {
	rows, err := s.db.QueryContext(ctx,
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

func (s *Store) listDesiredServices(ctx context.Context, agentID string) ([]*agentv1.DesiredService, error) {
	volumes, err := s.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetEnvironmentId(), vol.GetName())] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.environment_id, s.name, a.desired_spec_revision, a.desired_rollout_generation,
		        ro.image_digest, r.spec_json, e.network_identity, e.name, p.id, p.name,
		        a.restart_observation_json, a.operator_restart_nonce, a.rollout_state, a.drain_deadline,
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
		var drainDeadline sql.NullTime
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.EnvironmentId, &svc.Name, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &resolvedImage, &rawSpec, &networkIdentity, &environmentName, &projectID, &projectName, &restartRaw, &svc.OperatorRestartNonce, &rolloutState, &drainDeadline, &svc.PrivateIpv4, &svc.PrivateIpv6); err != nil {
			return nil, err
		}
		if rolloutState == allocationRolloutLost {
			continue
		}
		svc.Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_RUN
		if rolloutState == allocationRolloutDraining {
			svc.Intent = agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN
			if drainDeadline.Valid {
				svc.DrainDeadline = ts(drainDeadline.Time)
			}
		}
		if networkIdentity <= 0 || networkIdentity > int64(^uint32(0)) {
			return nil, fmt.Errorf("environment %s has invalid network identity %d", svc.EnvironmentId, networkIdentity)
		}
		svc.NetworkIdentity = uint32(networkIdentity)
		spec, err := loadServiceSpec(rawSpec)
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
			"PLATFORM_DEPLOYMENT_ID": deploymentRecordIDForRollout(svc.ServiceId, svc.DesiredRolloutGeneration),
		}
		for key, value := range platformEnv {
			svc.Spec.Runtime.Env[key] = value
		}
		if volumeName := serviceVolumeName(spec); volumeName != "" {
			svc.VolumeId = volumeIDs[volumeKey(svc.EnvironmentId, volumeName)]
		}
		svc.InternalHostname = internalServiceHostname(svc.Name, svc.ServiceId)
		svc.InternalHosts, err = s.internalHostsForEnvironment(ctx, svc.EnvironmentId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) internalHostsForEnvironment(ctx context.Context, environmentID string) ([]*agentv1.InternalHost, error) {
	rows, err := s.db.QueryContext(ctx,
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
			Hostname: internalServiceHostname(name, serviceID),
			Ipv4:     ipv4,
			Ipv6:     ipv6,
		})
	}
	return hosts, rows.Err()
}

func (s *Store) domainTargetPortsForService(ctx context.Context, serviceID string) ([]int32, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT target_port
		   FROM domain_bindings
		  WHERE service_id = $1
		  ORDER BY hostname ASC`,
		serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ports []int32
	for rows.Next() {
		var port int32
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, rows.Err()
}

func (s *Store) listHealthyIngressBackends(ctx context.Context) ([]ingressBackend, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.hostname, d.target_port, a.healthy_ipv4_ports, a.healthy_ipv6_ports,
		        a.allocation_ipv4, a.allocation_ipv6, a.id
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		   JOIN services s ON s.id = a.service_id
		  WHERE a.healthy = TRUE
		    AND a.rollout_state = $1
		    AND a.applied_spec_revision >= a.desired_spec_revision
		    AND a.applied_rollout_generation >= a.desired_rollout_generation
		    AND a.phase <> $2
		  ORDER BY d.hostname ASC, a.id ASC`,
		allocationRolloutServing,
		restartpolicy.PhaseCrashLoop,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
	for rows.Next() {
		var (
			domain           string
			targetPort       int32
			healthyIPv4Ports []int32
			healthyIPv6Ports []int32
			allocationIPv4   string
			allocationIPv6   string
			allocationID     string
		)
		if err := rows.Scan(&domain, &targetPort, (*jsonInt32Slice)(&healthyIPv4Ports), (*jsonInt32Slice)(&healthyIPv6Ports), &allocationIPv4, &allocationIPv6, &allocationID); err != nil {
			return nil, err
		}
		if slices.Contains(healthyIPv4Ports, targetPort) && net.ParseIP(allocationIPv4) != nil {
			backends = append(backends, ingressBackend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv4, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		} else if slices.Contains(healthyIPv6Ports, targetPort) && net.ParseIP(allocationIPv6) != nil {
			backends = append(backends, ingressBackend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv6, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		}
	}
	return backends, rows.Err()
}

func equalInt32Slices(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type ingressBackend struct {
	Domain       string
	Upstream     string
	AllocationID string
}
