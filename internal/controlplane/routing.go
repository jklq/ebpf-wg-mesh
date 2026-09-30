package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/restartpolicy"

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
			if _, err := journal.DomainRow(attemptHostname).Exec(ctx, tx,
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
		if _, err := journal.DomainRow(attemptHostname).Exec(ctx, tx,
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
		if _, err := journal.DomainRow(existing.Hostname).Exec(ctx, tx,
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

		if !binding.PlatformGenerated {
			generated, err := queryGeneratedDomainHostnames(ctx, tx, binding.ServiceID)
			if err != nil {
				return err
			}
			for _, platformHostname := range generated {
				if _, err := s.clearDomainBindingTombstoneTx(ctx, tx, platformHostname); err != nil {
					return err
				}

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
	result, err := journal.DomainRow(hostname).Exec(ctx, tx,
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
	result, err := journal.DomainRow(hostname).Exec(ctx, tx,
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

func (s *catalogPersistence) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, error) {
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	var binding deliverycore.DomainBindingRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var environmentID string
		if err := tx.QueryRowContext(ctx, `SELECT s.environment_id FROM allocations a JOIN services s ON s.id = a.service_id
			JOIN environments e ON e.id = s.environment_id WHERE s.id = $1 AND e.project_id = $2`, serviceID, projectID).Scan(&environmentID); err != nil {
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
			if _, err := journal.DomainRow(hostname).Exec(ctx, tx,
				`INSERT INTO domain_bindings(hostname, service_id, target_port, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, serviceID, targetPort, now, now,
			); err != nil {
				return err
			}
			binding = deliverycore.DomainBindingRecord{
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
			return deliverycore.ErrDomainAlreadyExists
		case binding.ServiceID == serviceID && binding.TargetPort == targetPort:
			return nil
		default:
			if _, err := journal.DomainRow(hostname).Exec(ctx, tx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        updated_at = $3
				  WHERE hostname = $4`,
				serviceID, targetPort, now, hostname,
			); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.TargetPort = targetPort
			binding.UpdatedAt = now
			return nil
		}
	})
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return binding, nil
}

type ingressLiveReader interface {
	Publishing() bool
	Product() *journal.Projection
	OverlayAllocation(deliverycore.AllocationRecord) deliverycore.AllocationRecord
}

func (s *routingPersistence) HealthyIngressBackends(ctx context.Context) ([]xds.Backend, error) {
	_ = ctx
	live := s.live
	if live == nil || !live.Publishing() {
		return nil, nil
	}
	return healthyIngressBackends(live.Product(), live), nil
}

func healthyIngressBackends(product *journal.Projection, live ingressLiveReader) []xds.Backend {
	durable := product.DurableState
	type row struct {
		hostname, allocationID, ipv4, ipv6 string
		port                               int32
		ipv4Ports, ipv6Ports               []int32
	}
	var rows []row
	for _, domain := range durable.Domains {
		for _, id := range product.AssignmentIDsForService(domain.ServiceID) {
			assignment := durable.Assignments[id]
			rec := live.OverlayAllocation(deliverycore.AllocationRecord{
				ID: assignment.ID, ServiceID: assignment.ServiceID, AgentID: assignment.AgentID,
				DesiredSpecRevision: assignment.DesiredSpecRevision, DesiredRolloutGeneration: assignment.DesiredRolloutGeneration,
				AllocationIPv4: assignment.AllocationIPv4, AllocationIPv6: assignment.AllocationIPv6,
				RolloutState: assignment.RolloutState, Message: assignment.IntentMessage,
			})
			if !rec.Healthy || rec.RolloutState != deliverycore.AllocationRolloutServing ||
				rec.AppliedSpecRevision < rec.DesiredSpecRevision || rec.AppliedRolloutGeneration < rec.DesiredRolloutGeneration ||
				rec.Phase == restartpolicy.PhaseCrashLoop {
				continue
			}
			rows = append(rows, row{
				hostname: domain.Hostname, allocationID: rec.ID, ipv4: rec.AllocationIPv4, ipv6: rec.AllocationIPv6,
				port: int32(domain.TargetPort), ipv4Ports: rec.HealthyIPv4Ports, ipv6Ports: rec.HealthyIPv6Ports,
			})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		if n := strings.Compare(a.hostname, b.hostname); n != 0 {
			return n
		}
		return strings.Compare(a.allocationID, b.allocationID)
	})
	var backends []xds.Backend
	for _, item := range rows {
		if slices.Contains(item.ipv4Ports, item.port) && net.ParseIP(item.ipv4) != nil {
			backends = append(backends, xds.Backend{
				Domain: item.hostname, Upstream: net.JoinHostPort(item.ipv4, strconv.Itoa(int(item.port))), AllocationID: item.allocationID,
			})
		} else if slices.Contains(item.ipv6Ports, item.port) && net.ParseIP(item.ipv6) != nil {
			backends = append(backends, xds.Backend{
				Domain: item.hostname, Upstream: net.JoinHostPort(item.ipv6, strconv.Itoa(int(item.port))), AllocationID: item.allocationID,
			})
		}
	}
	return backends
}

func (s *database) UpsertNodeObservations(ctx context.Context, observations []xds.NodeObservation) error {
	now := time.Now().UTC()
	for _, observation := range observations {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO xds_node_observations(node_id, applied_version, nacks, last_nack, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (node_id) DO UPDATE SET
				applied_version = CASE WHEN EXCLUDED.applied_version = '' THEN xds_node_observations.applied_version ELSE EXCLUDED.applied_version END,
				nacks = GREATEST(xds_node_observations.nacks, EXCLUDED.nacks),
				last_nack = CASE WHEN EXCLUDED.last_nack = '' THEN xds_node_observations.last_nack ELSE EXCLUDED.last_nack END,
				updated_at = EXCLUDED.updated_at`,
			observation.NodeID, observation.AppliedVersion, observation.NACKs, observation.LastNACK, now,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *database) ListNodeObservations(ctx context.Context) ([]xds.NodeObservation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, applied_version, nacks, last_nack FROM xds_node_observations ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []xds.NodeObservation
	for rows.Next() {
		var observation xds.NodeObservation
		if err := rows.Scan(&observation.NodeID, &observation.AppliedVersion, &observation.NACKs, &observation.LastNACK); err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return observations, nil
}

func (s *database) LoadPublication(ctx context.Context) (xds.Publication, error) {
	var pub xds.Publication
	err := s.db.QueryRowContext(ctx, `SELECT version, inputs, publisher FROM xds_publications WHERE id = TRUE`).Scan(
		&pub.Version, &pub.Inputs, &pub.Publisher,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return xds.Publication{}, nil
	}
	if err != nil {
		return xds.Publication{}, err
	}
	return pub, nil
}

func (s *database) CompareAndSwapPublication(ctx context.Context, oldVersion string, pub xds.Publication) (bool, error) {
	now := time.Now().UTC()
	if oldVersion == "" {
		result, err := s.db.ExecContext(ctx, `INSERT INTO xds_publications(
				id, version, inputs, publisher, updated_at
			) VALUES (TRUE, $1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			pub.Version, pub.Inputs, pub.Publisher, now,
		)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		return affected == 1, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE xds_publications
		SET version = $1, inputs = $2, publisher = $3, updated_at = $4
		WHERE id = TRUE AND version = $5`,
		pub.Version, pub.Inputs, pub.Publisher, now, oldVersion,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
