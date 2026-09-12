package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/routing"

	"github.com/jackc/pgx/v5/pgconn"
)

func (s *routingPersistence) CreateDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	scope, err := s.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	return s.putDomainBinding(ctx, scope, hostname, targetPort, false, true)
}

func (s *routingPersistence) CreatePlatformDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	scope, err := s.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	return s.putDomainBinding(ctx, scope, hostname, targetPort, true, true)
}

func (s *routingPersistence) UpdateDomainBindingRecord(ctx context.Context, user authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	binding, err := s.authz.AuthorizeDomainBinding(ctx, user, hostname, authz.Write)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	service, err := s.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	return s.updateDomainBinding(ctx, binding, service, targetPort)
}

func (s *routingPersistence) putDomainBinding(ctx context.Context, service authz.Service, hostname string, targetPort int32, platformGenerated, createOnly bool) (deliverycore.DomainBindingRecord, bool, error) {
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	var binding deliverycore.DomainBindingRecord
	var changed bool
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed = false
		binding = deliverycore.DomainBindingRecord{}
		attemptHostname, attemptCreateOnly := hostname, createOnly
		attemptPlatformGenerated := platformGenerated
		if attemptPlatformGenerated {
			existing, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service)
			if err == nil {
				attemptHostname, attemptCreateOnly = existing.Hostname, false
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		now := time.Now().UTC()
		var existing deliverycore.DomainBindingRecord
		err := tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			  WHERE d.hostname = $1 AND e.project_id = $2`,
			attemptHostname, service.ProjectID(),
		).Scan(&existing.Hostname, &existing.ProjectID, &existing.EnvironmentID, &existing.ServiceID, &existing.TargetPort, &existing.PlatformGenerated, &existing.CreatedAt, &existing.UpdatedAt)
		switch {
		case err == nil:
			if attemptCreateOnly {
				return deliverycore.ErrDomainAlreadyExists
			}
			if existing.ServiceID != service.ID() {
				return routing.ErrPlatformDomainReassignment
			}
			attemptPlatformGenerated = existing.PlatformGenerated
			binding = existing
			if existing.TargetPort == targetPort {
				return nil
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        platform_generated = $3,
				        updated_at = $4
				  WHERE hostname = $5`,
				service.ID(), targetPort, attemptPlatformGenerated, now, attemptHostname,
			); err != nil {
				return err
			}
			journal.RecordDomain(ctx, attemptHostname, service.ID())
			binding.TargetPort = targetPort
			binding.PlatformGenerated = attemptPlatformGenerated
			binding.UpdatedAt = now
			changed = true
			return nil
		case err != sql.ErrNoRows:
			return err
		}
		if !attemptPlatformGenerated {
			if _, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service); errors.Is(err, sql.ErrNoRows) {
				return routing.ErrPlatformDomainNotGenerated
			} else if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO domain_bindings(hostname, service_id, target_port, platform_generated, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			attemptHostname, service.ID(), targetPort, attemptPlatformGenerated, now, now,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return deliverycore.ErrDomainAlreadyExists
			}
			return err
		}
		journal.RecordDomain(ctx, attemptHostname, service.ID())
		binding = deliverycore.DomainBindingRecord{
			Hostname:          attemptHostname,
			ProjectID:         service.ProjectID(),
			EnvironmentID:     service.EnvironmentID(),
			ServiceID:         service.ID(),
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

func (s *routingPersistence) updateDomainBinding(ctx context.Context, binding authz.DomainBinding, service authz.Service, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	var out deliverycore.DomainBindingRecord
	var changed bool
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed = false
		out = deliverycore.DomainBindingRecord{}
		existing, err := s.domainBindingByScopeQuerier(ctx, tx, binding)
		if err != nil {
			return err
		}
		if existing.PlatformGenerated && existing.ServiceID != service.ID() {
			return routing.ErrPlatformDomainReassignment
		}
		if !existing.PlatformGenerated && existing.ServiceID != service.ID() {
			if _, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service); errors.Is(err, sql.ErrNoRows) {
				return routing.ErrPlatformDomainNotGenerated
			} else if err != nil {
				return err
			}
		}
		out = existing
		if existing.ServiceID == service.ID() && existing.TargetPort == targetPort {
			return nil
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE domain_bindings
			    SET service_id = $1,
			        target_port = $2,
			        platform_generated = $3,
			        updated_at = $4
			  WHERE hostname = $5`,
			service.ID(), targetPort, existing.PlatformGenerated, now, existing.Hostname,
		); err != nil {
			return err
		}
		journal.RecordDomain(ctx, existing.Hostname, service.ID())
		if existing.ServiceID != service.ID() {
			journal.RecordDomain(ctx, existing.Hostname, existing.ServiceID)
		}
		out.ServiceID = service.ID()
		out.ProjectID = service.ProjectID()
		out.EnvironmentID = service.EnvironmentID()
		out.TargetPort = targetPort
		out.UpdatedAt = now
		changed = true
		return nil
	})
	if err != nil {
		return deliverycore.DomainBindingRecord{}, false, err
	}
	return out, changed, nil
}

func (s *routingPersistence) DomainBindingByHostname(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error) {
	scope, err := s.authz.AuthorizeDomainBinding(ctx, user, hostname, authz.Read)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return s.domainBindingByScopeQuerier(ctx, s.db, scope)
}

func (s *routingPersistence) domainBindingByScopeQuerier(ctx context.Context, q deliverycore.ServiceQueryer, scope authz.DomainBinding) (deliverycore.DomainBindingRecord, error) {
	var binding deliverycore.DomainBindingRecord
	err := q.QueryRowContext(ctx,
		`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port,
		        d.platform_generated, d.created_at, d.updated_at
		   FROM domain_bindings d JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE d.hostname = $1 AND e.project_id = $2`,
		scope.Hostname(), scope.ProjectID(),
	).Scan(&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return binding, nil
}

func (s *routingPersistence) PlatformDomainBindingForService(ctx context.Context, user authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
	scope, err := s.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return s.platformDomainBindingForServiceQuerier(ctx, s.db, scope)
}

func (s *routingPersistence) platformDomainBindingForServiceQuerier(ctx context.Context, q deliverycore.ServiceQueryer, service authz.Service) (deliverycore.DomainBindingRecord, error) {
	var binding deliverycore.DomainBindingRecord
	err := q.QueryRowContext(ctx,
		`SELECT hostname, service_id, target_port, platform_generated, created_at, updated_at
		   FROM domain_bindings d
		  WHERE service_id = $1 AND platform_generated = TRUE`,
		service.ID(),
	).Scan(&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt)
	binding.ProjectID = service.ProjectID()
	binding.EnvironmentID = service.EnvironmentID()
	return binding, err
}

func (s *routingPersistence) DeleteDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (bool, error) {
	scope, err := s.authz.AuthorizeDomainBinding(ctx, user, hostname, authz.Write)
	if err != nil {
		return false, err
	}
	var changed bool
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed = false
		binding, err := s.domainBindingByScopeQuerier(ctx, tx, scope)
		if err != nil {
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
		result, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE hostname = $1`, binding.Hostname)
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
		journal.RecordDomain(ctx, binding.Hostname, binding.ServiceID)
		if !binding.PlatformGenerated {
			generated, err := queryGeneratedDomainHostnames(ctx, tx, binding.ServiceID)
			if err != nil {
				return err
			}
			for _, platformHostname := range generated {
				journal.RecordDomain(ctx, platformHostname, binding.ServiceID)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM domain_bindings WHERE service_id = $1 AND platform_generated AND NOT EXISTS (SELECT 1 FROM domain_bindings WHERE service_id = $1 AND NOT platform_generated)`, binding.ServiceID); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *readsPersistence) allocationByServiceID(ctx context.Context, serviceID string) (deliverycore.AllocationRecord, error) {
	allocs, err := s.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return deliverycore.AllocationRecord{}, err
	}
	return primaryAllocation(allocs), nil
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

func queryGeneratedDomainHostnames(ctx context.Context, tx *sql.Tx, serviceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT hostname FROM domain_bindings WHERE service_id = $1 AND platform_generated`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hostnames []string
	for rows.Next() {
		var hostname string
		if err := rows.Scan(&hostname); err != nil {
			return nil, err
		}
		hostnames = append(hostnames, hostname)
	}
	return hostnames, rows.Err()
}
