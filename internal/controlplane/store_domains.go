package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
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
		if deleted, err := serviceEffectivelyDeletedTx(ctx, tx, service.ID()); err != nil {
			return err
		} else if deleted {
			return deliverycore.ErrServiceDeleted
		}
		attemptHostname, attemptCreateOnly := hostname, createOnly
		attemptPlatformGenerated := platformGenerated
		if attemptPlatformGenerated {
			existing, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service)
			if err == nil {
				if existing.Deletion != nil {
					return deliverycore.ErrDomainAlreadyExists
				}
				attemptHostname, attemptCreateOnly = existing.Hostname, false
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		now := time.Now().UTC()
		var existing deliverycore.DomainBindingRecord
		var self, svc, environment, project deliverycore.Tombstone
		targets := []any{&existing.Hostname, &existing.ProjectID, &existing.EnvironmentID, &existing.ServiceID, &existing.TargetPort, &existing.PlatformGenerated, &existing.CreatedAt, &existing.UpdatedAt}
		targets = deliverycore.ScanTombstone(targets, &self)
		targets = deliverycore.ScanTombstone(targets, &svc)
		targets = deliverycore.ScanTombstone(targets, &environment)
		err := tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at,
			        d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
			        s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
			        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
			        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			   JOIN projects p ON p.id = e.project_id
			  WHERE d.hostname = $1 AND e.project_id = $2`,
			attemptHostname, service.ProjectID(),
		).Scan(deliverycore.ScanTombstone(targets, &project)...)
		if err == nil {
			existing.Deletion = deliverycore.EffectiveDeletion(self, svc, environment, project)
		}
		switch {
		case err == nil:
			if existing.Deletion != nil {
				return deliverycore.ErrDomainAlreadyExists
			}
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
			generated, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service)
			if errors.Is(err, sql.ErrNoRows) || err == nil && generated.Deletion != nil {
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
		if existing.Deletion != nil {
			return deliverycore.ErrDomainDeleted
		}
		if deleted, err := serviceEffectivelyDeletedTx(ctx, tx, service.ID()); err != nil {
			return err
		} else if deleted {
			return deliverycore.ErrServiceDeleted
		}
		if existing.PlatformGenerated && existing.ServiceID != service.ID() {
			return routing.ErrPlatformDomainReassignment
		}
		if !existing.PlatformGenerated && existing.ServiceID != service.ID() {
			generated, err := s.platformDomainBindingForServiceQuerier(ctx, tx, service)
			if errors.Is(err, sql.ErrNoRows) || err == nil && generated.Deletion != nil {
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
	var self, service, environment, project deliverycore.Tombstone
	targets := []any{&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt}
	targets = deliverycore.ScanTombstone(targets, &self)
	targets = deliverycore.ScanTombstone(targets, &service)
	targets = deliverycore.ScanTombstone(targets, &environment)
	err := q.QueryRowContext(ctx,
		`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port,
		        d.platform_generated, d.created_at, d.updated_at,
		        d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
		        s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM domain_bindings d JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE d.hostname = $1 AND e.project_id = $2`,
		scope.Hostname(), scope.ProjectID(),
	).Scan(deliverycore.ScanTombstone(targets, &project)...)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	binding.Deletion = deliverycore.EffectiveDeletion(self, service, environment, project)
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
	var self, svc, environment, project deliverycore.Tombstone
	targets := []any{&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt}
	targets = deliverycore.ScanTombstone(targets, &self)
	targets = deliverycore.ScanTombstone(targets, &svc)
	targets = deliverycore.ScanTombstone(targets, &environment)
	err := q.QueryRowContext(ctx,
		`SELECT d.hostname, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at,
		        d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
		        s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM domain_bindings d
		   JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE d.service_id = $1 AND d.platform_generated = TRUE`,
		service.ID(),
	).Scan(deliverycore.ScanTombstone(targets, &project)...)
	binding.ProjectID = service.ProjectID()
	binding.EnvironmentID = service.EnvironmentID()
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	binding.Deletion = deliverycore.EffectiveDeletion(self, svc, environment, project)
	return binding, nil
}

// DeleteDomainBindingRecord tombstones a binding instead of destroying it.
// Repeats are idempotent and report no change. Deleting the last live
// custom binding also tombstones the service's platform-generated binding,
// preserving the invariant that generated bindings exist only while custom
// bindings do; restoring a custom binding restores the generated one.
func (s *routingPersistence) DeleteDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (bool, error) {
	scope, err := s.authz.AuthorizeDomainBinding(ctx, user, hostname, authz.Write)
	if err != nil {
		return false, err
	}
	var changed bool
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changed = false
		binding, err := s.lockDomainBindingTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if binding.Deletion != nil && !binding.Deletion.Inherited {
			return nil
		}
		if binding.PlatformGenerated {
			var hasCustom bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM domain_bindings WHERE service_id = $1 AND NOT platform_generated AND deleted_at IS NULL)`, binding.ServiceID).Scan(&hasCustom); err != nil {
				return err
			}
			if hasCustom {
				return routing.ErrPlatformDomainInUse
			}
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		tombstoned, err := s.tombstoneDomainBindingTx(ctx, tx, binding.Hostname, user.ID(), now)
		if err != nil {
			return err
		}
		if !tombstoned {
			return nil
		}
		journal.RecordDomain(ctx, binding.Hostname, binding.ServiceID)
		if !binding.PlatformGenerated {
			generated, err := queryGeneratedDomainHostnames(ctx, tx, binding.ServiceID)
			if err != nil {
				return err
			}
			var liveCustom bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM domain_bindings WHERE service_id = $1 AND NOT platform_generated AND deleted_at IS NULL)`, binding.ServiceID).Scan(&liveCustom); err != nil {
				return err
			}
			if !liveCustom {
				for _, platformHostname := range generated {
					if _, err := s.tombstoneDomainBindingTx(ctx, tx, platformHostname, user.ID(), now); err != nil {
						return err
					}
					journal.RecordDomain(ctx, platformHostname, binding.ServiceID)
				}
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

// RestoreDomainBindingRecord clears a binding's own tombstone within the
// grace period. Restoring a custom binding also restores the service's
// platform-generated binding, which cannot outlive the last custom binding.
// Restoring under a tombstoned ancestor is refused: restore top-down.
func (s *routingPersistence) RestoreDomainBindingRecord(ctx context.Context, user authz.User, hostname string) (deliverycore.DomainBindingRecord, error) {
	scope, err := s.authz.AuthorizeDomainBinding(ctx, user, hostname, authz.Write)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	var restored deliverycore.DomainBindingRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		binding, err := s.lockDomainBindingTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		if binding.Deletion == nil {
			restored = binding
			return nil
		}
		if binding.Deletion.Inherited {
			return deliverycore.ErrAncestorDeleted
		}
		cleared, err := s.clearDomainBindingTombstoneTx(ctx, tx, binding.Hostname)
		if err != nil {
			return err
		}
		if !cleared {
			return deliverycore.ErrDeletionExpired
		}
		journal.RecordDomain(ctx, binding.Hostname, binding.ServiceID)
		if !binding.PlatformGenerated {
			generated, err := queryGeneratedDomainHostnames(ctx, tx, binding.ServiceID)
			if err != nil {
				return err
			}
			for _, platformHostname := range generated {
				if _, err := s.clearDomainBindingTombstoneTx(ctx, tx, platformHostname); err != nil {
					return err
				}
				journal.RecordDomain(ctx, platformHostname, binding.ServiceID)
			}
		}
		restored, err = s.domainBindingByScopeQuerier(ctx, tx, scope)
		return err
	})
	return restored, err
}

func (s *routingPersistence) lockDomainBindingTx(ctx context.Context, tx *sql.Tx, scope authz.DomainBinding) (deliverycore.DomainBindingRecord, error) {
	var binding deliverycore.DomainBindingRecord
	var self, service, environment, project deliverycore.Tombstone
	targets := []any{&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt}
	targets = deliverycore.ScanTombstone(targets, &self)
	targets = deliverycore.ScanTombstone(targets, &service)
	targets = deliverycore.ScanTombstone(targets, &environment)
	err := tx.QueryRowContext(ctx,
		`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port,
		        d.platform_generated, d.created_at, d.updated_at,
		        d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
		        s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM domain_bindings d JOIN services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE d.hostname = $1 AND e.project_id = $2 FOR UPDATE OF d`,
		scope.Hostname(), scope.ProjectID(),
	).Scan(deliverycore.ScanTombstone(targets, &project)...)
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	binding.Deletion = deliverycore.EffectiveDeletion(self, service, environment, project)
	return binding, nil
}

func (s *routingPersistence) tombstoneDomainBindingTx(ctx context.Context, tx *sql.Tx, hostname, userID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE domain_bindings
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE hostname = $4 AND deleted_at IS NULL`,
		now, userID, now.Add(s.deletionGracePeriod()), hostname,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *routingPersistence) clearDomainBindingTombstoneTx(ctx context.Context, tx *sql.Tx, hostname string) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE domain_bindings
		    SET deleted_at = NULL,
		        deleted_by_user_id = '',
		        delete_expires_at = NULL
		  WHERE hostname = $1 AND deleted_at IS NOT NULL AND delete_expires_at > statement_timestamp()`,
		hostname,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
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

// serviceEffectivelyDeletedTx reports whether a service is tombstoned itself
// or sits under a tombstoned environment or project.
func serviceEffectivelyDeletedTx(ctx context.Context, tx *sql.Tx, serviceID string) (bool, error) {
	var deleted bool
	err := tx.QueryRowContext(ctx,
		`SELECT s.deleted_at IS NOT NULL OR e.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE s.id = $1`,
		serviceID,
	).Scan(&deleted)
	if err != nil {
		return false, err
	}
	return deleted, nil
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
