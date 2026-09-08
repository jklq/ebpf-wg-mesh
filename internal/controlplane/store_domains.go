package controlplane

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/routing"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"time"
)

func (s *routingPersistence) CreateDomainBindingRecord(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, false, true)
}

func (s *routingPersistence) CreatePlatformDomainBindingRecord(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, true, true)
}

func (s *routingPersistence) UpdateDomainBindingRecord(ctx context.Context, userID, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	return s.putDomainBinding(ctx, userID, hostname, serviceID, targetPort, false, false)
}

func (s *routingPersistence) putDomainBinding(ctx context.Context, userID, hostname, serviceID string, targetPort int32, platformGenerated, createOnly bool) (deliverycore.DomainBindingRecord, bool, error) {
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	var binding deliverycore.DomainBindingRecord
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		changed = false
		binding = deliverycore.DomainBindingRecord{}
		attemptHostname, attemptCreateOnly := hostname, createOnly
		attemptPlatformGenerated := platformGenerated
		service, err := s.serviceLocation(ctx, tx, userID, serviceID)
		if err != nil {
			return err
		}
		if err := s.authorizeEnvironmentWrite(ctx, tx, userID, service.EnvironmentID); err != nil {
			return err
		}
		if attemptPlatformGenerated {
			existing, err := s.platformDomainBindingForServiceQuerier(ctx, tx, userID, serviceID)
			if err == nil {
				attemptHostname, attemptCreateOnly = existing.Hostname, false
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		now := time.Now().UTC()
		var existing deliverycore.DomainBindingRecord
		err = tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			  WHERE d.hostname = $1`,
			attemptHostname,
		).Scan(&existing.Hostname, &existing.ProjectID, &existing.EnvironmentID, &existing.ServiceID, &existing.TargetPort, &existing.PlatformGenerated, &existing.CreatedAt, &existing.UpdatedAt)
		switch {
		case err == nil:
			if attemptCreateOnly {
				return deliverycore.ErrDomainAlreadyExists
			}
			if err := s.authorizeEnvironmentWrite(ctx, tx, userID, existing.EnvironmentID); err != nil {
				return err
			}
			if existing.PlatformGenerated && existing.ServiceID != serviceID {
				return routing.ErrPlatformDomainReassignment
			}
			if !existing.PlatformGenerated && existing.ServiceID != serviceID {
				if _, err := s.platformDomainBindingForServiceQuerier(ctx, tx, userID, serviceID); errors.Is(err, sql.ErrNoRows) {
					return routing.ErrPlatformDomainNotGenerated
				} else if err != nil {
					return err
				}
			}
			attemptPlatformGenerated = existing.PlatformGenerated
			binding = existing
			if existing.ServiceID == serviceID && existing.TargetPort == targetPort && existing.PlatformGenerated == attemptPlatformGenerated {
				return nil
			}
			agentIDs, err := s.agentIDsForService(ctx, tx, serviceID)
			if err != nil {
				return err
			}
			if existing.ServiceID != serviceID {
				previousIDs, err := s.agentIDsForService(ctx, tx, existing.ServiceID)
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
				serviceID, targetPort, attemptPlatformGenerated, now, attemptHostname,
			); err != nil {
				return err
			}
			if err := dbtx.BumpDesiredRevisions(ctx, tx, agentIDs); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.ProjectID = service.ProjectID
			binding.EnvironmentID = service.EnvironmentID
			binding.TargetPort = targetPort
			binding.PlatformGenerated = attemptPlatformGenerated
			binding.UpdatedAt = now
			changed = true
			return nil
		case err != sql.ErrNoRows:
			return err
		}
		if !attemptCreateOnly {
			return sql.ErrNoRows
		}
		if !attemptPlatformGenerated {
			if _, err := s.platformDomainBindingForServiceQuerier(ctx, tx, userID, serviceID); errors.Is(err, sql.ErrNoRows) {
				return routing.ErrPlatformDomainNotGenerated
			} else if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO domain_bindings(hostname, service_id, target_port, platform_generated, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			attemptHostname, serviceID, targetPort, attemptPlatformGenerated, now, now,
		); err != nil {
			return err
		}
		createAgentIDs, err := s.agentIDsForService(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if err := dbtx.BumpDesiredRevisions(ctx, tx, createAgentIDs); err != nil {
			return err
		}
		binding = deliverycore.DomainBindingRecord{
			Hostname:          attemptHostname,
			ProjectID:         service.ProjectID,
			EnvironmentID:     service.EnvironmentID,
			ServiceID:         serviceID,
			TargetPort:        targetPort,
			PlatformGenerated: attemptPlatformGenerated,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		changed = true
		return nil
	})
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	return binding, changed, nil
}

func (s *routingPersistence) DomainBindingByHostname(ctx context.Context, userID, hostname string) (deliverycore.DomainBindingRecord, error) {
	return s.domainBindingByHostnameQuerier(ctx, s.db, userID, hostname)
}

func (s *routingPersistence) domainBindingByHostnameQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, hostname string) (deliverycore.DomainBindingRecord, error) {
	var binding deliverycore.DomainBindingRecord
	err := q.QueryRowContext(ctx,
		`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port,
		        d.platform_generated, d.created_at, d.updated_at
		   FROM domain_bindings d JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN project_memberships m ON m.project_id = e.project_id
		  WHERE d.hostname = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor', 'viewer')`,
		hostname, userID,
	).Scan(&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return binding, nil
}

func (s *routingPersistence) PlatformDomainBindingForService(ctx context.Context, userID, serviceID string) (deliverycore.DomainBindingRecord, error) {
	return s.platformDomainBindingForServiceQuerier(ctx, s.db, userID, serviceID)
}

func (s *routingPersistence) platformDomainBindingForServiceQuerier(ctx context.Context, q deliverycore.ServiceQueryer, userID, serviceID string) (deliverycore.DomainBindingRecord, error) {
	service, err := s.serviceLocation(ctx, q, userID, serviceID)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	var binding deliverycore.DomainBindingRecord
	err = q.QueryRowContext(ctx,
		`SELECT hostname, service_id, target_port, platform_generated, created_at, updated_at
		   FROM domain_bindings d
		  WHERE service_id = $1 AND platform_generated = TRUE`,
		serviceID,
	).Scan(&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	binding.ProjectID = service.ProjectID
	binding.EnvironmentID = service.EnvironmentID
	return binding, err
}

func (s *routingPersistence) DeleteDomainBindingRecord(ctx context.Context, userID, hostname string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		changed = false
		binding, err := s.domainBindingByHostnameQuerier(ctx, tx, userID, hostname)
		if err != nil {
			return err
		}
		if err := s.authorizeEnvironmentWrite(ctx, tx, userID, binding.EnvironmentID); err != nil {
			return err
		}
		if binding.PlatformGenerated {
			var hasCustom bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM domain_bindings WHERE service_id = $1 AND NOT platform_generated)`, binding.ServiceID).Scan(&hasCustom); err != nil {
				return err
			}
			if hasCustom {
				return routing.ErrPlatformDomainInUse
			}
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
		agentIDs, err := s.agentIDsForService(ctx, tx, serviceID)
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
		if !binding.PlatformGenerated {
			if _, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE service_id = $1 AND platform_generated AND NOT EXISTS (SELECT 1 FROM domain_bindings WHERE service_id = $1 AND NOT platform_generated)`, serviceID); err != nil {
				return err
			}
		}
		if err := dbtx.BumpDesiredRevisions(ctx, tx, agentIDs); err != nil {
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

// allocationByServiceID returns the first allocation row associated with a
// service, if any. A missing allocation is treated as a non-error empty record
// so callers that just want stage projections can keep going without
// special-casing the not-yet-scheduled path.
func (s *readsPersistence) allocationByServiceID(ctx context.Context, serviceID string) (deliverycore.AllocationRecord, error) {
	allocs, err := s.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return deliverycore.AllocationRecord{}, err
	}
	return primaryAllocation(allocs), nil
}

type serviceLocation struct {
	ID, ProjectID, EnvironmentID string
}

func (s *routingPersistence) authorizeEnvironmentWrite(ctx context.Context, q deliverycore.ServiceQueryer, userID, environmentID string) error {
	var one int
	return q.QueryRowContext(ctx, `SELECT 1 FROM environments e JOIN projects p ON p.id = e.project_id JOIN project_memberships m ON m.project_id = e.project_id WHERE e.id = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor') AND p.kind = $3`, environmentID, userID, string(deliverycore.ProjectKindUser)).Scan(&one)
}

func (s *routingPersistence) agentIDsForService(ctx context.Context, q deliverycore.ServiceQueryer, serviceID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT agent_id FROM allocations WHERE service_id = $1 AND agent_id <> '' ORDER BY agent_id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *routingPersistence) serviceLocation(ctx context.Context, q deliverycore.ServiceQueryer, userID, serviceID string) (serviceLocation, error) {
	var rec serviceLocation
	err := q.QueryRowContext(ctx, `SELECT s.id, e.project_id, s.environment_id FROM services s
 JOIN environments e ON e.id = s.environment_id
 JOIN project_memberships m ON m.project_id = e.project_id
 WHERE s.id = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor', 'viewer')`, serviceID, userID).Scan(&rec.ID, &rec.ProjectID, &rec.EnvironmentID)
	return rec, err
}
