package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	errVolumeInUse         = errors.New("volume still referenced by service")
	errVolumeNotFound      = errors.New("volume not found")
	errVolumeAgentMismatch = errors.New("volume bound to different agent")
	errConcurrentUpdate    = errors.New("concurrent service update")
	errDomainAlreadyExists = errors.New("domain binding already exists")
)

type serviceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func canonicalServiceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSpec {
	if spec == nil {
		return nil
	}
	out := proto.Clone(spec).(*platformv1.ServiceSpec)
	if len(out.Command) == 0 {
		out.Command = nil
	}
	if len(out.Args) == 0 {
		out.Args = nil
	}
	if len(out.Env) == 0 {
		out.Env = nil
	}
	if hc := out.GetHealthCheck(); hc != nil &&
		hc.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED &&
		hc.GetPath() == "" &&
		hc.GetPort() == 0 &&
		hc.GetIntervalSeconds() == 0 &&
		hc.GetTimeoutSeconds() == 0 {
		out.HealthCheck = nil
	}
	return out
}

type placementCandidate struct {
	ID                     string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func sortedDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	sort.Strings(out)
	return out
}

func sameServiceSpec(a, b *platformv1.ServiceSpec) bool {
	return proto.Equal(canonicalServiceSpec(a), canonicalServiceSpec(b))
}

func loadServiceSpec(raw []byte) (*platformv1.ServiceSpec, error) {
	spec := &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(raw, spec); err != nil {
		return nil, err
	}
	return canonicalServiceSpec(spec), nil
}

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
	if spec != nil && spec.GetVolumeName() != "" {
		volumeAgentID, err := s.boundAgentForVolumeQuerier(ctx, tx, projectID, spec.GetVolumeName())
		if err != nil {
			return serviceRecord{}, err
		}
		if volumeAgentID != agentID {
			return serviceRecord{}, fmt.Errorf("%w: volume %q is bound to %s, service is scheduled to %s", errVolumeAgentMismatch, spec.GetVolumeName(), volumeAgentID, agentID)
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
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, err
	}
	allocationID := mustID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rec.ID, rec.ProjectID, rec.Name, rec.SpecRevision, rec.RolloutGeneration, rec.AllocatedAgentID, now, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, rec.RolloutGeneration, rec.SpecRevision, "create", "", "", now); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO allocations(
			id, service_id, project_id, agent_id,
			desired_spec_revision, applied_spec_revision,
			desired_rollout_generation, applied_rollout_generation,
			phase, message, endpoint_addr, healthy, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		allocationID, rec.ID, rec.ProjectID, rec.AllocatedAgentID, rec.SpecRevision, 0, rec.RolloutGeneration, 0, "Pending", "", "", false, now,
	); err != nil {
		return serviceRecord{}, err
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
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", now); err != nil {
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
		if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID}); err != nil {
			return err
		}
		rec = current
		rec.Spec = spec
		rec.SpecRevision = nextSpecRevision
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string) (domainBindingRecord, error) {
	var binding domainBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var existingServiceID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&existingServiceID); err != nil {
			return err
		}

		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx,
			`SELECT hostname, project_id, service_id, created_at, updated_at
			   FROM domain_bindings
			  WHERE hostname = $1`,
			hostname,
		).Scan(&binding.Hostname, &binding.ProjectID, &binding.ServiceID, &binding.CreatedAt, &binding.UpdatedAt)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, project_id, service_id, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, projectID, serviceID, now, now,
			); err != nil {
				return err
			}
			binding = domainBindingRecord{
				Hostname:  hostname,
				ProjectID: projectID,
				ServiceID: serviceID,
				CreatedAt: now,
				UpdatedAt: now,
			}
			return nil
		case err != nil:
			return err
		case binding.ProjectID != projectID:
			return errDomainAlreadyExists
		case binding.ServiceID == serviceID:
			return nil
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        updated_at = $2
				  WHERE hostname = $3 AND project_id = $4`,
				serviceID, now, hostname, projectID,
			); err != nil {
				return err
			}
			binding.ServiceID = serviceID
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
	if spec != nil && spec.GetVolumeName() != "" {
		return s.boundAgentForVolumeQuerier(ctx, tx, projectID, spec.GetVolumeName())
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
			if candidate.CPUMillisCapacity > 0 && candidate.UsedCPUMillis+spec.GetCpuMillis() > candidate.CPUMillisCapacity {
				continue
			}
			if candidate.MemoryMebibytesCapcity > 0 && candidate.UsedMemoryMebibytes+spec.GetMemoryMebibytes() > candidate.MemoryMebibytesCapcity {
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
		               COALESCE(SUM(COALESCE((r.spec_json->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
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
		volumeIDs[vol.GetProjectId()+"\x00"+vol.GetName()] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.project_id, s.name, s.current_spec_revision, s.current_rollout_generation, r.spec_json
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE a.agent_id = $1
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
		var rawSpec []byte
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.ProjectId, &svc.Name, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &rawSpec); err != nil {
			return nil, err
		}
		spec, err := loadServiceSpec(rawSpec)
		if err != nil {
			return nil, err
		}
		svc.Spec = spec
		if svc.Spec.VolumeName != "" {
			svc.VolumeId = volumeIDs[svc.ProjectId+"\x00"+svc.Spec.VolumeName]
		}
		svc.PrivateIpv6, err = privateIPv6(agent.WorkloadIPv6Subnet, svc.ProjectId, svc.ServiceId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) listHealthyIngressBackends(ctx context.Context) ([]ingressBackend, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.hostname, a.endpoint_addr
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		  WHERE a.healthy = TRUE AND a.endpoint_addr <> ''
		  ORDER BY d.hostname ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
	for rows.Next() {
		var backend ingressBackend
		if err := rows.Scan(&backend.Domain, &backend.EndpointAddr); err != nil {
			return nil, err
		}
		backends = append(backends, backend)
	}
	return backends, rows.Err()
}

func (s *Store) markAllocationHealthyForTest(ctx context.Context, serviceID, endpointAddr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE allocations SET healthy = TRUE, endpoint_addr = $1, updated_at = $2 WHERE service_id = $3`,
		endpointAddr, time.Now().UTC(), serviceID,
	)
	return err
}

func (s *Store) countServiceRevisionsForTest(ctx context.Context, serviceID string) (int, error) {
	var revisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revisions); err != nil {
		return 0, err
	}
	return revisions, nil
}

func (s *Store) countServiceRolloutsForTest(ctx context.Context, serviceID string) (int, error) {
	var rollouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_rollouts WHERE service_id = $1`, serviceID).Scan(&rollouts); err != nil {
		return 0, err
	}
	return rollouts, nil
}

type ingressBackend struct {
	Domain       string
	EndpointAddr string
}
