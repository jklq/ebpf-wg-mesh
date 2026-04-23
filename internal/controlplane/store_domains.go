package controlplane

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) createDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, subject, projectID, hostname, serviceID, targetPort, true)
}

func (s *Store) updateDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, subject, projectID, hostname, serviceID, targetPort, false)
}

func (s *Store) putDomainBinding(ctx context.Context, subject, projectID, hostname, serviceID string, targetPort int32, createOnly bool) (domainBindingRecord, bool, error) {
	if err := validatePort(targetPort); err != nil {
		return domainBindingRecord{}, false, err
	}
	var binding domainBindingRecord
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, subject, projectID, serviceID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var existing domainBindingRecord
		err = tx.QueryRowContext(ctx,
			`SELECT hostname, project_id, service_id, target_port, created_at, updated_at
			   FROM domain_bindings
			  WHERE hostname = $1`,
			hostname,
		).Scan(&existing.Hostname, &existing.ProjectID, &existing.ServiceID, &existing.TargetPort, &existing.CreatedAt, &existing.UpdatedAt)
		switch {
		case err == nil:
			if existing.ProjectID != projectID {
				return sql.ErrNoRows
			}
			if createOnly {
				return errDomainAlreadyExists
			}
			binding = existing
			if existing.ServiceID == serviceID && existing.TargetPort == targetPort {
				return nil
			}
			agentIDs := []string{service.AllocatedAgentID}
			if existing.ServiceID != serviceID {
				var previousAgentID string
				if err := tx.QueryRowContext(ctx, `SELECT allocated_agent_id FROM services WHERE id = $1`, existing.ServiceID).Scan(&previousAgentID); err != nil {
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
			changed = true
			return nil
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO domain_bindings(hostname, project_id, service_id, target_port, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			hostname, projectID, serviceID, targetPort, now, now,
		); err != nil {
			return err
		}
		if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{service.AllocatedAgentID}); err != nil {
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
		changed = true
		return nil
	})
	if err != nil {
		return domainBindingRecord{}, false, err
	}
	return binding, changed, nil
}

func (s *Store) domainBindingByHostname(ctx context.Context, subject, projectID, hostname string) (domainBindingRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return domainBindingRecord{}, err
	}
	var binding domainBindingRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT hostname, project_id, service_id, target_port, created_at, updated_at
		   FROM domain_bindings
		  WHERE hostname = $1 AND project_id = $2`,
		hostname, projectID,
	).Scan(&binding.Hostname, &binding.ProjectID, &binding.ServiceID, &binding.TargetPort, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return domainBindingRecord{}, err
	}
	return binding, nil
}

func (s *Store) listDomainBindings(ctx context.Context, subject, projectID, serviceID string) ([]domainBindingRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return nil, err
	}
	query := `SELECT hostname, project_id, service_id, target_port, created_at, updated_at
	            FROM domain_bindings
	           WHERE project_id = $1`
	args := []any{projectID}
	if serviceID != "" {
		query += ` AND service_id = $2`
		args = append(args, serviceID)
	}
	query += ` ORDER BY hostname ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domainBindingRecord
	for rows.Next() {
		var binding domainBindingRecord
		if err := rows.Scan(&binding.Hostname, &binding.ProjectID, &binding.ServiceID, &binding.TargetPort, &binding.CreatedAt, &binding.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *Store) deleteDomainBinding(ctx context.Context, subject, projectID, hostname string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
			return err
		}
		var agentID string
		if err := tx.QueryRowContext(ctx,
			`SELECT s.allocated_agent_id
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			  WHERE d.hostname = $1 AND d.project_id = $2`,
			hostname, projectID,
		).Scan(&agentID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE hostname = $1 AND project_id = $2`, hostname, projectID)
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
		if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{agentID}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) serviceStatus(ctx context.Context, subject, projectID, serviceID string) (serviceRecord, allocationRecord, error) {
	service, err := s.serviceByID(ctx, subject, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, allocationRecord{}, err
	}
	var alloc allocationRecord
	err = s.db.QueryRowContext(ctx,
		`SELECT id, service_id, project_id, agent_id, desired_spec_revision, applied_spec_revision, phase, message, allocation_ip, healthy, updated_at, desired_rollout_generation, applied_rollout_generation, healthy_ports
		   FROM allocations
		  WHERE service_id = $1`,
		serviceID,
	).Scan(
		&alloc.ID,
		&alloc.ServiceID,
		&alloc.ProjectID,
		&alloc.AgentID,
		&alloc.DesiredSpecRevision,
		&alloc.AppliedSpecRevision,
		&alloc.Phase,
		&alloc.Message,
		&alloc.AllocationIP,
		&alloc.Healthy,
		&alloc.UpdatedAt,
		&alloc.DesiredRolloutGeneration,
		&alloc.AppliedRolloutGeneration,
		(*jsonInt32Slice)(&alloc.HealthyPorts),
	)
	if err != nil {
		return serviceRecord{}, allocationRecord{}, err
	}
	return service, alloc, nil
}
