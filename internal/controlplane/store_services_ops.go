package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) createVolumeTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, sizeBytes int64) (volumeRecord, error) {
	environment, err := s.environmentByIDQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return volumeRecord{}, err
	}
	rec := volumeRecord{
		ID:            mustID(),
		EnvironmentID: environment.ID,
		Name:          name,
		SizeBytes:     sizeBytes,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, environment_id, name, size_bytes, created_at) VALUES ($1, $2, $3, $4, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.SizeBytes, rec.CreatedAt,
	); err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createServiceTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	environment, err := s.environmentByIDQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return serviceRecord{}, err
	}
	return s.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID)
}

func (s *Store) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return serviceRecord{}, err
	}
	environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err != nil {
		return serviceRecord{}, err
	}
	return s.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID)
}

func (s *Store) createStagedServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	return s.insertServiceTx(ctx, tx, environment, name, spec, "")
}

func (s *Store) createDeployedServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if agentID == "" {
		return serviceRecord{}, errors.New("agent id required")
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, environment.ID, volumeName); err != nil {
			return serviceRecord{}, err
		}
	}
	if err := validateVolumeReplicaCompatibility(spec, defaultDesiredReplicaCount); err != nil {
		return serviceRecord{}, err
	}
	rec, err := s.insertServiceTx(ctx, tx, environment, name, spec, agentID)
	if err != nil {
		return serviceRecord{}, err
	}
	now := rec.CreatedAt
	rec.RolloutGeneration = 1
	rec.ResolvedImage = directImageRef(spec)
	if _, err := tx.ExecContext(ctx, `UPDATE services
		SET current_rollout_generation = 1, current_resolved_image = $1 WHERE id = $2`, rec.ResolvedImage, rec.ID); err != nil {
		return serviceRecord{}, err
	}
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, 1, 1, "create", "", "", now); err != nil {
		return serviceRecord{}, err
	}
	rec.DesiredReplicaCount = defaultDesiredReplicaCount
	if _, err := s.reconcileServiceReplicasTx(ctx, tx, rec, agentID, now); err != nil {
		return serviceRecord{}, err
	}
	if desiredSourceSpec(spec) != nil {
		if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
			return serviceRecord{}, err
		}
	}
	// Allocations are part of every node's workload identity catalog. Creating
	// one therefore changes mesh policy globally even though only one agent runs
	// the workload.
	if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) insertServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	now := time.Now().UTC()
	spec = canonicalServiceSpec(spec)
	rec := serviceRecord{
		ID:                  mustID(),
		EnvironmentID:       environment.ID,
		ProjectID:           environment.ProjectID,
		Name:                strings.TrimSpace(name),
		Spec:                spec,
		SpecRevision:        1,
		AllocatedAgentID:    agentID,
		DesiredReplicaCount: defaultDesiredReplicaCount,
		CreatedAt:           now,
		UpdatedAt:           now,
		PendingChanges:      true,
	}
	if source := desiredSourceSpec(spec); source != nil {
		rec.SourceSummary = toProtoSourceStateSummary(source, nil, nil, nil)
	} else {
		rec.SourceSummary = buildSourceSummary(spec)
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(
			id, environment_id, name, current_spec_revision, current_rollout_generation,
			current_resolved_image, last_successful_commit_sha, latest_build_id,
			desired_replica_count, placement_message, created_at, updated_at
		) VALUES ($1, $2, $3, 1, 0, '', '', '', $4, '', $5, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.DesiredReplicaCount, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedService(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec, trustedAgentID string) (serviceRecord, []string, error) {
	var rec serviceRecord
	var affectedAgentIDs []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		trustedAgentID = strings.TrimSpace(trustedAgentID)
		if trustedAgentID == "" {
			return errors.New("trusted agent id required for managed service")
		}
		var agentExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1)`, trustedAgentID).Scan(&agentExists); err != nil {
			return err
		}
		if !agentExists {
			return fmt.Errorf("%w: trusted agent %s is not enrolled", errNoPlacementAvailable, trustedAgentID)
		}
		environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
			WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
		if err != nil {
			return err
		}
		var hasOtherWorkloads bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM allocations a JOIN services s ON s.id = a.service_id
				 WHERE a.agent_id = $1
				   AND NOT (s.environment_id = $2 AND s.name = $3)
			)`,
			trustedAgentID, environment.ID, name,
		).Scan(&hasOtherWorkloads); err != nil {
			return err
		}
		if hasOtherWorkloads {
			return fmt.Errorf("%w: trusted agent %s is not dedicated to the managed dashboard", errNoPlacementAvailable, trustedAgentID)
		}
		current, found, err := s.serviceByNameQuerier(ctx, tx, environment.ID, name)
		if err != nil {
			return err
		}
		if !found {
			rec, err = s.createServiceTxInternal(ctx, tx, projectID, name, spec, trustedAgentID)
			affectedAgentIDs = []string{trustedAgentID}
			return err
		}
		spec = canonicalServiceSpec(spec)
		placementChanged := current.AllocatedAgentID != trustedAgentID
		if sameServiceSpec(current.Spec, spec) && !placementChanged {
			rec = current
			return nil
		}
		now := time.Now().UTC()
		nextSpecRevision := current.SpecRevision + 1
		nextRolloutGeneration := current.RolloutGeneration + 1
		specJSON, err := protojson.Marshal(spec)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE services
				    SET current_spec_revision = $1,
				        current_rollout_generation = $2,
				        updated_at = $3
				  WHERE id = $4`,
			nextSpecRevision,
			nextRolloutGeneration,
			now,
			current.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			current.ID,
			nextSpecRevision,
			specJSON,
			now,
		); err != nil {
			return err
		}
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE allocations
			    SET desired_spec_revision = $1,
			        desired_rollout_generation = $2,
			        agent_id = $3,
			        phase = $4,
			        message = $5,
			        healthy = $6,
			        updated_at = $7
			  WHERE service_id = $8`,
			nextSpecRevision,
			nextRolloutGeneration,
			trustedAgentID,
			"Pending",
			"",
			false,
			now,
			current.ID,
		); err != nil {
			return err
		}
		rec = current
		rec.Spec = spec
		rec.SpecRevision = nextSpecRevision
		rec.RolloutGeneration = nextRolloutGeneration
		rec.AllocatedAgentID = trustedAgentID
		rec.UpdatedAt = now
		if desiredSourceSpec(spec) != nil {
			if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
				return err
			}
		}
		if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
			return err
		}
		affectedAgentIDs = []string{current.AllocatedAgentID, trustedAgentID}
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, nil, err
	}
	if len(affectedAgentIDs) == 0 {
		return rec, nil, nil
	}
	allAgentIDs, err := s.agentIDs(ctx)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	return rec, allAgentIDs, nil
}

func (s *Store) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, error) {
	if err := validatePort(targetPort); err != nil {
		return domainBindingRecord{}, err
	}
	var binding domainBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var agentID, environmentID string
		if err := tx.QueryRowContext(ctx, `SELECT a.agent_id, s.environment_id FROM allocations a JOIN services s ON s.id = a.service_id
			JOIN environments e ON e.id = s.environment_id WHERE s.id = $1 AND e.project_id = $2`, serviceID, projectID).Scan(&agentID, &environmentID); err != nil {
			return err
		}

		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.created_at, d.updated_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			  WHERE d.hostname = $1`,
			hostname,
		).Scan(&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.CreatedAt, &binding.UpdatedAt)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, service_id, target_port, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, serviceID, targetPort, now, now,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{agentID}); err != nil {
				return err
			}
			binding = domainBindingRecord{
				Hostname:      hostname,
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ServiceID:     serviceID,
				TargetPort:    targetPort,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			return nil
		case err != nil:
			return err
		case binding.ProjectID != projectID:
			return errDomainAlreadyExists
		case binding.ServiceID == serviceID && binding.TargetPort == targetPort:
			return nil
		default:
			agentIDs := []string{agentID}
			if binding.ServiceID != serviceID {
				previousIDs, err := s.agentIDsForServiceQuerier(ctx, tx, binding.ServiceID)
				if err != nil {
					return err
				}
				agentIDs = append(agentIDs, previousIDs...)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        updated_at = $3
				  WHERE hostname = $4`,
				serviceID, targetPort, now, hostname,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, agentIDs); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.TargetPort = targetPort
			binding.UpdatedAt = now
			return nil
		}
	})
	if err != nil {
		return domainBindingRecord{}, err
	}
	return binding, nil
}

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
		        a.cpu_millis_capacity,
		        a.memory_mebibytes_capacity,
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
		  WHERE a.last_seen_at > $1
		  ORDER BY COALESCE(stats.service_count, 0) ASC, a.id ASC`,
		time.Now().UTC().Add(-30*time.Second),
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
	agent, err := s.agentByID(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumes, err := s.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetEnvironmentId(), vol.GetName())] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.environment_id, s.name, s.current_spec_revision, s.current_rollout_generation,
		        s.current_resolved_image, r.spec_json, e.network_identity, e.name, p.id, p.name
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE a.agent_id = $1
		    AND s.current_resolved_image <> ''
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
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.EnvironmentId, &svc.Name, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &resolvedImage, &rawSpec, &networkIdentity, &environmentName, &projectID, &projectName); err != nil {
			return nil, err
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
		svc.PrivateIpv6, err = privateIPv6(agent.WorkloadIPv6Subnet, svc.EnvironmentId, svc.AllocationId)
		if err != nil {
			return nil, err
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
		`SELECT s.id, s.name, a.id, ag.workload_ipv6_subnet, a.allocation_ip
		   FROM services s
		   JOIN allocations a ON a.service_id = s.id
		   JOIN agents ag ON ag.id = a.agent_id
		  WHERE s.environment_id = $1
		    AND a.healthy = TRUE
		    AND a.applied_spec_revision >= s.current_spec_revision
		    AND a.applied_rollout_generation >= s.current_rollout_generation
		  ORDER BY s.created_at ASC, a.id ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []*agentv1.InternalHost
	for rows.Next() {
		var serviceID, name, allocationID, workloadSubnet, reportedIP string
		if err := rows.Scan(&serviceID, &name, &allocationID, &workloadSubnet, &reportedIP); err != nil {
			return nil, err
		}
		ipv6 := strings.TrimSpace(reportedIP)
		if !s.useReportedAllocationIP || net.ParseIP(ipv6) == nil {
			derived, err := privateIPv6(workloadSubnet, environmentID, allocationID)
			if err != nil {
				return nil, err
			}
			ipv6 = derived
		}
		hosts = append(hosts, &agentv1.InternalHost{
			Hostname: internalServiceHostname(name, serviceID),
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
		`SELECT d.hostname, d.target_port, a.healthy_ports, a.allocation_ip, ag.workload_ipv6_subnet, s.environment_id, a.id
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		   JOIN services s ON s.id = a.service_id
		   JOIN agents ag ON ag.id = a.agent_id
		  WHERE a.healthy = TRUE
		    AND a.applied_spec_revision >= s.current_spec_revision
		    AND a.applied_rollout_generation >= s.current_rollout_generation
		  ORDER BY d.hostname ASC, a.id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
	for rows.Next() {
		var (
			domain         string
			targetPort     int32
			healthyPorts   []int32
			reportedIP     string
			workloadSubnet string
			environmentID  string
			allocationID   string
		)
		if err := rows.Scan(&domain, &targetPort, (*jsonInt32Slice)(&healthyPorts), &reportedIP, &workloadSubnet, &environmentID, &allocationID); err != nil {
			return nil, err
		}
		if !slices.Contains(healthyPorts, targetPort) {
			continue
		}
		allocationIP := strings.TrimSpace(reportedIP)
		if s.useReportedAllocationIP {
			if net.ParseIP(allocationIP) == nil {
				return nil, fmt.Errorf("reported ingress allocation ip %q is invalid", allocationIP)
			}
		} else {
			var err error
			allocationIP, err = privateIPv6(workloadSubnet, environmentID, allocationID)
			if err != nil {
				return nil, fmt.Errorf("derive ingress allocation ip: %w", err)
			}
		}
		backends = append(backends, ingressBackend{
			Domain:   domain,
			Upstream: net.JoinHostPort(allocationIP, strconv.Itoa(int(targetPort))),
		})
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
	Domain   string
	Upstream string
}
