package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) createVolumeTx(ctx context.Context, tx *sql.Tx, subject, projectID, name string, sizeBytes int64, agentID string) (volumeRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
		return volumeRecord{}, err
	}
	rec := volumeRecord{
		ID:           mustID(),
		ProjectID:    projectID,
		Name:         name,
		SizeBytes:    sizeBytes,
		BoundAgentID: agentID,
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, project_id, name, size_bytes, bound_agent_id, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.ID, rec.ProjectID, rec.Name, rec.SizeBytes, rec.BoundAgentID, rec.CreatedAt,
	); err != nil {
		return volumeRecord{}, err
	}
	if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{rec.BoundAgentID}); err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createServiceTx(ctx context.Context, tx *sql.Tx, subject, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
		return serviceRecord{}, err
	}
	return s.createServiceTxInternal(ctx, tx, projectID, name, spec, agentID)
}

func (s *Store) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return serviceRecord{}, err
	}
	if agentID == "" {
		return serviceRecord{}, errors.New("agent id required")
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		volumeAgentID, err := s.boundAgentForVolumeQuerier(ctx, tx, projectID, volumeName)
		if err != nil {
			return serviceRecord{}, err
		}
		if volumeAgentID != agentID {
			return serviceRecord{}, fmt.Errorf("%w: volume %q is bound to %s, service is scheduled to %s", errVolumeAgentMismatch, volumeName, volumeAgentID, agentID)
		}
	}

	now := time.Now().UTC()
	spec = canonicalServiceSpec(spec)
	rec := serviceRecord{
		ID:                mustID(),
		ProjectID:         projectID,
		Name:              name,
		Spec:              spec,
		SpecRevision:      1,
		RolloutGeneration: 1,
		AllocatedAgentID:  agentID,
		ResolvedImage:     directImageRef(spec),
		CreatedAt:         now,
		UpdatedAt:         now,
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
	allocationID := mustID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(
			id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id,
			current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		rec.ID, rec.ProjectID, rec.Name, rec.SpecRevision, rec.RolloutGeneration, rec.AllocatedAgentID, rec.ResolvedImage, rec.LastSuccessfulCommitSHA, rec.LatestBuildID, now, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, rec.RolloutGeneration, rec.SpecRevision, "create", "", "", "", now); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO allocations(
			id, service_id, project_id, agent_id,
			desired_spec_revision, applied_spec_revision,
			desired_rollout_generation, applied_rollout_generation,
			phase, message, allocation_ip, healthy_ports, healthy, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		allocationID, rec.ID, rec.ProjectID, rec.AllocatedAgentID, rec.SpecRevision, 0, rec.RolloutGeneration, 0, "Pending", "", "", []byte("[]"), false, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if desiredSourceSpec(spec) != nil {
		if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
			return serviceRecord{}, err
		}
	}
	if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{rec.AllocatedAgentID}); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedService(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.serviceByNameQuerier(ctx, tx, projectID, name)
		if err != nil {
			return err
		}
		if !found {
			agentID, err := s.chooseAgentForServiceTx(ctx, tx, projectID, spec)
			if err != nil {
				return err
			}
			rec, err = s.createServiceTxInternal(ctx, tx, projectID, name, spec, agentID)
			return err
		}
		spec = canonicalServiceSpec(spec)
		if sameServiceSpec(current.Spec, spec) {
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
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", "", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE allocations
			    SET desired_spec_revision = $1,
			        desired_rollout_generation = $2,
			        phase = $3,
			        message = $4,
			        healthy = $5,
			        updated_at = $6
			  WHERE service_id = $7`,
			nextSpecRevision,
			nextRolloutGeneration,
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
		rec.UpdatedAt = now
		if desiredSourceSpec(spec) != nil {
			if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
				return err
			}
		}
		if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID}); err != nil {
			return err
		}
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
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
		var agentID string
		if err := tx.QueryRowContext(ctx, `SELECT allocated_agent_id FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&agentID); err != nil {
			return err
		}

		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx,
			`SELECT hostname, project_id, service_id, target_port, created_at, updated_at
			   FROM domain_bindings
			  WHERE hostname = $1`,
			hostname,
		).Scan(&binding.Hostname, &binding.ProjectID, &binding.ServiceID, &binding.TargetPort, &binding.CreatedAt, &binding.UpdatedAt)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, project_id, service_id, target_port, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				hostname, projectID, serviceID, targetPort, now, now,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{agentID}); err != nil {
				return err
			}
			binding = domainBindingRecord{
				Hostname:   hostname,
				ProjectID:  projectID,
				ServiceID:  serviceID,
				TargetPort: targetPort,
				CreatedAt:  now,
				UpdatedAt:  now,
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
				var previousAgentID string
				if err := tx.QueryRowContext(ctx, `SELECT allocated_agent_id FROM services WHERE id = $1`, binding.ServiceID).Scan(&previousAgentID); err != nil {
					return err
				}
				agentIDs = append(agentIDs, previousAgentID)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        updated_at = $3
				  WHERE hostname = $4 AND project_id = $5`,
				serviceID, targetPort, now, hostname, projectID,
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

func (s *Store) chooseAgentForVolumeTx(ctx context.Context, tx *sql.Tx) (string, error) {
	return s.chooseAgentForPlacementQuerier(ctx, tx, nil)
}

func (s *Store) chooseAgentForServiceTx(ctx context.Context, tx *sql.Tx, projectID string, spec *platformv1.ServiceSpec) (string, error) {
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		return s.boundAgentForVolumeQuerier(ctx, tx, projectID, volumeName)
	}
	return s.chooseAgentForPlacementQuerier(ctx, tx, spec)
}

func (s *Store) chooseAgentForPlacementQuerier(ctx context.Context, q serviceQueryer, spec *platformv1.ServiceSpec) (string, error) {
	candidates, err := s.placementCandidatesQuerier(ctx, q)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if spec != nil {
			runtime := serviceRuntime(spec)
			if candidate.CPUMillisCapacity > 0 && candidate.UsedCPUMillis+runtime.GetCpuMillis() > candidate.CPUMillisCapacity {
				continue
			}
			if candidate.MemoryMebibytesCapcity > 0 && candidate.UsedMemoryMebibytes+runtime.GetMemoryMebibytes() > candidate.MemoryMebibytesCapcity {
				continue
			}
		}
		return candidate.ID, nil
	}
	return "", errNoPlacementAvailable
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
		        SELECT s.allocated_agent_id AS agent_id,
		               COUNT(*) AS service_count,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
		          FROM services s
		          JOIN service_revisions r
		            ON r.service_id = s.id
		           AND r.spec_revision = s.current_spec_revision
		         GROUP BY s.allocated_agent_id
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

func (s *Store) boundAgentForVolume(ctx context.Context, projectID, volumeName string) (string, error) {
	return s.boundAgentForVolumeQuerier(ctx, s.db, projectID, volumeName)
}

func (s *Store) boundAgentForVolumeQuerier(ctx context.Context, q serviceQueryer, projectID, volumeName string) (string, error) {
	var agentID string
	err := q.QueryRowContext(ctx, `SELECT bound_agent_id FROM volumes WHERE project_id = $1 AND name = $2`, projectID, volumeName).Scan(&agentID)
	switch {
	case err == nil:
		return agentID, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %q", errVolumeNotFound, volumeName)
	default:
		return "", err
	}
}

func (s *Store) listDesiredVolumes(ctx context.Context, agentID string) ([]*agentv1.DesiredVolume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, size_bytes
		   FROM volumes
		  WHERE bound_agent_id = $1
		  ORDER BY created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentv1.DesiredVolume
	for rows.Next() {
		vol := &agentv1.DesiredVolume{}
		if err := rows.Scan(&vol.VolumeId, &vol.ProjectId, &vol.Name, &vol.SizeBytes); err != nil {
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
		volumeIDs[volumeKey(vol.GetProjectId(), vol.GetName())] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.project_id, s.name, s.current_spec_revision, s.current_rollout_generation, s.current_resolved_image, r.spec_json
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
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
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.ProjectId, &svc.Name, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &resolvedImage, &rawSpec); err != nil {
			return nil, err
		}
		spec, err := loadServiceSpec(rawSpec)
		if err != nil {
			return nil, err
		}
		targetPorts, err := s.domainTargetPortsForService(ctx, svc.ServiceId)
		if err != nil {
			return nil, err
		}
		svc.Spec = resolvedDesiredServiceSpec(spec, resolvedImage, targetPorts)
		if volumeName := serviceVolumeName(spec); volumeName != "" {
			svc.VolumeId = volumeIDs[volumeKey(svc.ProjectId, volumeName)]
		}
		svc.PrivateIpv6, err = privateIPv6(agent.WorkloadIPv6Subnet, svc.ProjectId, svc.ServiceId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
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
		`SELECT d.hostname, d.target_port, a.allocation_ip
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		  WHERE a.healthy = TRUE AND a.allocation_ip <> ''
		  ORDER BY d.hostname ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
	for rows.Next() {
		var (
			domain       string
			targetPort   int32
			allocationIP string
		)
		if err := rows.Scan(&domain, &targetPort, &allocationIP); err != nil {
			return nil, err
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
