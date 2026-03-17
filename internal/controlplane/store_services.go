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
)

var (
	errVolumeInUse         = errors.New("volume still referenced by service")
	errVolumeNotFound      = errors.New("volume not found")
	errVolumeAgentMismatch = errors.New("volume bound to different agent")
	errConcurrentUpdate    = errors.New("concurrent service update")
)

type serviceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sortedDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	sort.Strings(out)
	return out
}

func (s *Store) createVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64, agentID string) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createVolumeTx(ctx, tx, subject, projectID, name, sizeBytes, agentID)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledVolume(ctx context.Context, subject, projectID, name string, sizeBytes int64) (volumeRecord, error) {
	var rec volumeRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agentID, err := s.chooseAgentForVolumeTx(ctx, tx)
		if err != nil {
			return err
		}
		rec, err = s.createVolumeTx(ctx, tx, subject, projectID, name, sizeBytes, agentID)
		return err
	})
	if err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) listVolumes(ctx context.Context, subject, projectID string) ([]volumeRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, size_bytes, bound_agent_id, created_at
		   FROM volumes
		  WHERE project_id = $1
		  ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []volumeRecord
	for rows.Next() {
		var rec volumeRecord
		if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &rec.SizeBytes, &rec.BoundAgentID, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) deleteVolume(ctx context.Context, subject, projectID, volumeID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		var volumeName string
		err := tx.QueryRowContext(ctx, `SELECT name FROM volumes WHERE id = $1 AND project_id = $2`, volumeID, projectID).Scan(&volumeName)
		if err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, r.spec_json
			   FROM services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.revision = s.current_revision
			  WHERE s.project_id = $1`,
			projectID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var serviceID string
			var rawSpec []byte
			if err := rows.Scan(&serviceID, &rawSpec); err != nil {
				return err
			}
			spec := &platformv1.ServiceSpec{}
			if err := protojson.Unmarshal(rawSpec, spec); err != nil {
				return err
			}
			if spec.GetVolumeName() == volumeName {
				return fmt.Errorf("%w: service %s references volume %s", errVolumeInUse, serviceID, volumeID)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1 AND project_id = $2`, volumeID, projectID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		_, err = s.nextDesiredRevisionTx(ctx, tx)
		return err
	})
}

func (s *Store) createService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec, agentID string, domains []string) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		rec, err = s.createServiceTx(ctx, tx, subject, projectID, name, spec, agentID, domains)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) createScheduledService(ctx context.Context, subject, projectID, name string, spec *platformv1.ServiceSpec, domains []string) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		agentID, err := s.chooseAgentForServiceTx(ctx, tx, projectID, spec)
		if err != nil {
			return err
		}
		rec, err = s.createServiceTx(ctx, tx, subject, projectID, name, spec, agentID, domains)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) updateService(ctx context.Context, subject, projectID, serviceID string, spec *platformv1.ServiceSpec, domains []string) (serviceRecord, error) {
	var current serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		current, err = s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
		if err != nil {
			return err
		}
		if spec != nil && spec.GetVolumeName() != "" {
			volumeAgentID, err := s.boundAgentForVolumeQuerier(ctx, tx, projectID, spec.GetVolumeName())
			if err != nil {
				return err
			}
			if volumeAgentID != current.AllocatedAgentID {
				return fmt.Errorf("%w: volume %q is bound to %s, service is allocated to %s", errVolumeAgentMismatch, spec.GetVolumeName(), volumeAgentID, current.AllocatedAgentID)
			}
		}

		now := time.Now().UTC()
		nextRevision := current.CurrentRevision + 1
		specJSON, err := protojson.Marshal(spec)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE services SET current_revision = $1, updated_at = $2 WHERE id = $3 AND current_revision = $4`,
			nextRevision, now, serviceID, current.CurrentRevision,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return errConcurrentUpdate
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_revisions(service_id, revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			serviceID, nextRevision, specJSON, now,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE allocations
			    SET desired_revision = $1,
			        phase = $2,
			        message = $3,
			        healthy = $4,
			        updated_at = $5
			  WHERE service_id = $6`,
			nextRevision, "Pending", "", false, now, serviceID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM service_domains WHERE service_id = $1`, serviceID); err != nil {
			return err
		}
		for _, domain := range domains {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO service_domains(domain, project_id, service_id, created_at) VALUES ($1, $2, $3, $4)`,
				domain, projectID, serviceID, now,
			); err != nil {
				return err
			}
		}
		if _, err := s.nextDesiredRevisionTx(ctx, tx); err != nil {
			return err
		}

		current.Spec = spec
		current.CurrentRevision = nextRevision
		current.Domains = sortedDomains(domains)
		current.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return current, nil
}

func (s *Store) deleteService(ctx context.Context, subject, projectID, serviceID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return sql.ErrNoRows
		}
		_, err = s.nextDesiredRevisionTx(ctx, tx)
		return err
	})
}

func (s *Store) listServices(ctx context.Context, subject, projectID string) ([]serviceRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, current_revision, allocated_agent_id, created_at, updated_at
		   FROM services
		  WHERE project_id = $1
		  ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []serviceRecord
	for rows.Next() {
		rec, err := scanServiceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		spec, domains, err := s.loadServiceDetails(ctx, out[i].ID, out[i].CurrentRevision)
		if err != nil {
			return nil, err
		}
		out[i].Spec = spec
		out[i].Domains = domains
	}
	return out, nil
}

func (s *Store) serviceByID(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, error) {
	return s.serviceByIDQuerier(ctx, s.db, subject, projectID, serviceID)
}

func (s *Store) serviceByIDQuerier(ctx context.Context, q serviceQueryer, subject, projectID, serviceID string) (serviceRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, q, subject, projectID); err != nil {
		return serviceRecord{}, err
	}
	row := q.QueryRowContext(ctx,
		`SELECT id, project_id, name, current_revision, allocated_agent_id, created_at, updated_at
		   FROM services
		  WHERE id = $1 AND project_id = $2`,
		serviceID, projectID,
	)
	rec, err := scanServiceRow(row)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.Spec, rec.Domains, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.CurrentRevision)
	return rec, err
}

func scanServiceRow(scanner interface{ Scan(...any) error }) (serviceRecord, error) {
	var rec serviceRecord
	if err := scanner.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &rec.CurrentRevision, &rec.AllocatedAgentID, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) loadServiceDetails(ctx context.Context, serviceID string, revision int64) (*platformv1.ServiceSpec, []string, error) {
	return s.loadServiceDetailsQuerier(ctx, s.db, serviceID, revision)
}

func (s *Store) loadServiceDetailsQuerier(ctx context.Context, q serviceQueryer, serviceID string, revision int64) (*platformv1.ServiceSpec, []string, error) {
	var rawSpec []byte
	if err := q.QueryRowContext(ctx, `SELECT spec_json FROM service_revisions WHERE service_id = $1 AND revision = $2`, serviceID, revision).Scan(&rawSpec); err != nil {
		return nil, nil, err
	}
	spec := &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(rawSpec, spec); err != nil {
		return nil, nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT domain FROM service_domains WHERE service_id = $1 ORDER BY domain ASC`, serviceID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var domains []string
	for rows.Next() {
		var domain string
		if err := rows.Scan(&domain); err != nil {
			return nil, nil, err
		}
		domains = append(domains, domain)
	}
	return spec, domains, rows.Err()
}

func (s *Store) upsertDomain(ctx context.Context, subject, projectID, serviceID, domain string) (serviceRecord, error) {
	var service serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_domains(domain, project_id, service_id, created_at)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT(domain) DO UPDATE SET service_id = excluded.service_id, project_id = excluded.project_id`,
			domain, projectID, serviceID, time.Now().UTC(),
		); err != nil {
			return err
		}
		if _, err := s.nextDesiredRevisionTx(ctx, tx); err != nil {
			return err
		}
		var err error
		service, err = s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
		return err
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return service, nil
}

func (s *Store) deleteDomain(ctx context.Context, subject, projectID, domain string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM service_domains WHERE domain = $1 AND project_id = $2`, domain, projectID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return sql.ErrNoRows
		}
		_, err = s.nextDesiredRevisionTx(ctx, tx)
		return err
	})
}

func (s *Store) serviceStatus(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
	service, err := s.serviceByID(ctx, subject, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, allocationRecord{}, err
	}
	var alloc allocationRecord
	err = s.db.QueryRowContext(ctx,
		`SELECT id, service_id, project_id, agent_id, desired_revision, applied_revision, phase, message, endpoint_addr, healthy, updated_at
		   FROM allocations
		  WHERE service_id = $1`,
		serviceID,
	).Scan(
		&alloc.ID,
		&alloc.ServiceID,
		&alloc.ProjectID,
		&alloc.AgentID,
		&alloc.DesiredRevision,
		&alloc.AppliedRevision,
		&alloc.Phase,
		&alloc.Message,
		&alloc.EndpointAddr,
		&alloc.Healthy,
		&alloc.UpdatedAt,
	)
	if err != nil {
		return serviceRecord{}, allocationRecord{}, err
	}
	return service, alloc, nil
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
	if _, err := s.nextDesiredRevisionTx(ctx, tx); err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createServiceTx(ctx context.Context, tx *sql.Tx, subject, projectID, name string, spec *platformv1.ServiceSpec, agentID string, domains []string) (serviceRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
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
	rec := serviceRecord{
		ID:               mustID(),
		ProjectID:        projectID,
		Name:             name,
		Spec:             spec,
		CurrentRevision:  1,
		AllocatedAgentID: agentID,
		Domains:          sortedDomains(domains),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, err
	}
	allocationID := mustID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(id, project_id, name, current_revision, allocated_agent_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rec.ID, rec.ProjectID, rec.Name, rec.CurrentRevision, rec.AllocatedAgentID, now, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.CurrentRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO allocations(id, service_id, project_id, agent_id, desired_revision, applied_revision, phase, message, endpoint_addr, healthy, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		allocationID, rec.ID, rec.ProjectID, rec.AllocatedAgentID, rec.CurrentRevision, 0, "Pending", "", "", false, now,
	); err != nil {
		return serviceRecord{}, err
	}
	for _, domain := range rec.Domains {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_domains(domain, project_id, service_id, created_at) VALUES ($1, $2, $3, $4)`,
			domain, projectID, rec.ID, now,
		); err != nil {
			return serviceRecord{}, err
		}
	}
	if _, err := s.nextDesiredRevisionTx(ctx, tx); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) chooseAgentForVolumeTx(ctx context.Context, tx *sql.Tx) (string, error) {
	agents, services, err := s.schedulerSnapshotTx(ctx, tx)
	if err != nil {
		return "", err
	}
	return chooseAgent(agents, services, nil)
}

func (s *Store) chooseAgentForServiceTx(ctx context.Context, tx *sql.Tx, projectID string, spec *platformv1.ServiceSpec) (string, error) {
	if spec != nil && spec.GetVolumeName() != "" {
		return s.boundAgentForVolumeQuerier(ctx, tx, projectID, spec.GetVolumeName())
	}
	agents, services, err := s.schedulerSnapshotTx(ctx, tx)
	if err != nil {
		return "", err
	}
	return chooseAgent(agents, services, spec)
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
		`SELECT a.id, s.id, s.project_id, s.name, s.current_revision, s.current_revision, r.spec_json
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN service_revisions r ON r.service_id = s.id AND r.revision = s.current_revision
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
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.ProjectId, &svc.Name, &svc.DesiredRevision, &svc.DesiredRevision, &rawSpec); err != nil {
			return nil, err
		}
		svc.Spec = &platformv1.ServiceSpec{}
		if err := protojson.Unmarshal(rawSpec, svc.Spec); err != nil {
			return nil, err
		}
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
		`SELECT d.domain, a.endpoint_addr
		   FROM service_domains d
		   JOIN allocations a ON a.service_id = d.service_id
		  WHERE a.healthy = TRUE AND a.endpoint_addr <> ''
		  ORDER BY d.domain ASC`,
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

type ingressBackend struct {
	Domain       string
	EndpointAddr string
}
