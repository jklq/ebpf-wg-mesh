package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) createDomainBinding(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, false, true)
}

func (s *Store) createPlatformDomainBinding(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	if existing, err := s.platformDomainBindingForService(ctx, userID, serviceID); err == nil {
		return s.putDomainBinding(ctx, userID, existing.Hostname, serviceID, targetPort, true, false)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domainBindingRecord{}, false, err
	}
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, true, true)
}

func (s *Store) updateDomainBinding(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, false, false)
}

func (s *Store) putDomainBinding(ctx context.Context, userID, hostname, serviceID string, targetPort int32, platformGenerated, createOnly bool) (domainBindingRecord, bool, error) {
	if err := validatePort(targetPort); err != nil {
		return domainBindingRecord{}, false, err
	}
	var binding domainBindingRecord
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, userID, serviceID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, service.EnvironmentID); err != nil {
			return err
		}
		now := time.Now().UTC()
		var existing domainBindingRecord
		err = tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			  WHERE d.hostname = $1`,
			hostname,
		).Scan(&existing.Hostname, &existing.ProjectID, &existing.EnvironmentID, &existing.ServiceID, &existing.TargetPort, &existing.PlatformGenerated, &existing.CreatedAt, &existing.UpdatedAt)
		switch {
		case err == nil:
			if createOnly {
				return errDomainAlreadyExists
			}
			platformGenerated = existing.PlatformGenerated
			binding = existing
			if existing.ServiceID == serviceID && existing.TargetPort == targetPort && existing.PlatformGenerated == platformGenerated {
				return nil
			}
			agentIDs, err := s.agentIDsForServiceQuerier(ctx, tx, serviceID)
			if err != nil {
				return err
			}
			if existing.ServiceID != serviceID {
				previousIDs, err := s.agentIDsForServiceQuerier(ctx, tx, existing.ServiceID)
				if err != nil {
					return err
				}
				agentIDs = append(agentIDs, previousIDs...)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        platform_generated = $3,
				        updated_at = $4
				  WHERE hostname = $5`,
				serviceID, targetPort, platformGenerated, now, hostname,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, agentIDs); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.ProjectID = service.ProjectID
			binding.EnvironmentID = service.EnvironmentID
			binding.TargetPort = targetPort
			binding.PlatformGenerated = platformGenerated
			binding.UpdatedAt = now
			changed = true
			return nil
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO domain_bindings(hostname, service_id, target_port, platform_generated, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			hostname, serviceID, targetPort, platformGenerated, now, now,
		); err != nil {
			return err
		}
		createAgentIDs, err := s.agentIDsForServiceQuerier(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if err := s.bumpDesiredRevisionsTx(ctx, tx, createAgentIDs); err != nil {
			return err
		}
		binding = domainBindingRecord{
			Hostname:          hostname,
			ProjectID:         service.ProjectID,
			EnvironmentID:     service.EnvironmentID,
			ServiceID:         serviceID,
			TargetPort:        targetPort,
			PlatformGenerated: platformGenerated,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		changed = true
		return nil
	})
	if err != nil {
		return domainBindingRecord{}, false, err
	}
	return binding, changed, nil
}

func (s *Store) domainBindingByHostname(ctx context.Context, userID, hostname string) (domainBindingRecord, error) {
	var binding domainBindingRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port,
		        d.platform_generated, d.created_at, d.updated_at
		   FROM domain_bindings d JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN project_memberships m ON m.project_id = e.project_id
		  WHERE d.hostname = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor', 'viewer')`,
		hostname, userID,
	).Scan(&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return domainBindingRecord{}, err
	}
	return binding, nil
}

func (s *Store) listDomainBindings(ctx context.Context, userID, serviceID string) ([]domainBindingRecord, error) {
	service, err := s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT hostname, service_id, target_port, platform_generated, created_at, updated_at
		FROM domain_bindings WHERE service_id = $1 ORDER BY hostname ASC`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domainBindingRecord
	for rows.Next() {
		var binding domainBindingRecord
		binding.ProjectID = service.ProjectID
		binding.EnvironmentID = service.EnvironmentID
		if err := rows.Scan(&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *Store) platformDomainBindingForService(ctx context.Context, userID, serviceID string) (domainBindingRecord, error) {
	service, err := s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return domainBindingRecord{}, err
	}
	var binding domainBindingRecord
	err = s.db.QueryRowContext(ctx,
		`SELECT hostname, service_id, target_port, platform_generated, created_at, updated_at
		   FROM domain_bindings d
		  WHERE service_id = $1 AND platform_generated = TRUE`,
		serviceID,
	).Scan(&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	binding.ProjectID = service.ProjectID
	binding.EnvironmentID = service.EnvironmentID
	return binding, err
}

func (s *Store) deleteDomainBinding(ctx context.Context, userID, hostname string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		binding, err := s.domainBindingByHostname(ctx, userID, hostname)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, binding.EnvironmentID); err != nil {
			return err
		}
		var serviceID string
		if err := tx.QueryRowContext(ctx,
			`SELECT d.service_id
			   FROM domain_bindings d
			  WHERE d.hostname = $1`,
			hostname,
		).Scan(&serviceID); err != nil {
			return err
		}
		agentIDs, err := s.agentIDsForServiceQuerier(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE hostname = $1`, hostname)
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
		if err := s.bumpDesiredRevisionsTx(ctx, tx, agentIDs); err != nil {
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

func (s *Store) serviceStatus(ctx context.Context, userID, serviceID string) (serviceRecord, []allocationRecord, error) {
	service, err := s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	allocs, err := s.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	return service, allocs, nil
}

// allocationByServiceID returns the first allocation row associated with a
// service, if any. A missing allocation is treated as a non-error empty record
// so callers that just want stage projections can keep going without
// special-casing the not-yet-scheduled path.
func (s *Store) allocationByServiceID(ctx context.Context, serviceID string) (allocationRecord, error) {
	allocs, err := s.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return allocationRecord{}, err
	}
	return primaryAllocation(allocs), nil
}
