package delivery

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// CheckDeletionConfirmation requires a typed confirmation to exactly match
// the resource's current name.
func CheckDeletionConfirmation(current, confirmation string) error {
	if current == "" || strings.TrimSpace(confirmation) != current {
		return ErrConfirmationMismatch
	}
	return nil
}

// ServiceQuiescer performs service-level deletion quiesce inside a caller's
// product transaction. It lets catalog-owned operations (environment and
// project deletion) reuse delivery's deployment, build, and source-work
// transitions without duplicating them.
type ServiceQuiescer struct {
	store *persistence
}

// NewServiceQuiescer builds a quiescer over the delivery source store. The
// quiescer never touches the database outside the caller's transaction.
func NewServiceQuiescer(sourceStore SourceStore) *ServiceQuiescer {
	return &ServiceQuiescer{store: &persistence{sourceStore: sourceStore, deletionGrace: DefaultDeletionGracePeriod}}
}

// QuiesceTx stops new work for one service: its current deployment moves to
// Removed (which also cancels late builder completions), queued builds are
// cancelled, and pending source work is dropped. Running builds finish into
// the terminal deployment and are cancelled on completion.
func (q *ServiceQuiescer) QuiesceTx(ctx context.Context, tx *sql.Tx, serviceID, actorUserID string) error {
	return quiesceServiceTx(ctx, q.store, tx, serviceID, actorUserID)
}

func quiesceServiceTx(ctx context.Context, s *persistence, tx *sql.Tx, serviceID, actorUserID string) error {
	if err := s.markCurrentDeploymentRemovedTx(ctx, tx, serviceID, deploymentActor{Kind: DeploymentCauseUser, ID: actorUserID}); err != nil {
		return err
	}
	if err := s.cancelQueuedBuildsTx(ctx, tx, serviceID, "service deleted"); err != nil {
		return err
	}
	if s.sourceStore == nil {
		return nil
	}
	return s.sourceStore.DeletePendingServiceWorkTx(ctx, tx, serviceID)
}

func (s *persistence) cancelQueuedBuildsTx(ctx context.Context, tx *sql.Tx, serviceID, reason string) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx,
		`UPDATE build_runs
		    SET state = $1,
		        failure_reason = $2,
		        finished_at = $3
		  WHERE service_id = $4 AND state = $5`,
		BuildStateCancelled, reason, now, serviceID, BuildStateQueued,
	)
	return err
}

// lockServiceDeletionTx locks the service row and reports its effective
// deletion state: its own tombstone or the nearest tombstoned ancestor.
func (s *persistence) lockServiceDeletionTx(ctx context.Context, tx *sql.Tx, serviceID string) (*DeletionInfo, error) {
	var self, environment, project Tombstone
	targets := ScanTombstone(nil, &self)
	targets = ScanTombstone(targets, &environment)
	err := tx.QueryRowContext(ctx,
		`SELECT s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE s.id = $1 FOR UPDATE OF s`,
		serviceID,
	).Scan(ScanTombstone(targets, &project)...)
	if err != nil {
		return nil, err
	}
	return EffectiveDeletion(self, environment, project), nil
}

// serviceDeletionQuerier reports a service's effective deletion state without
// locking. Callers that mutate must lock first (lockServiceDeletionTx) and
// re-check, or hold the lock through an enclosing enumeration.
func (s *persistence) serviceDeletionQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (*DeletionInfo, error) {
	var self, environment, project Tombstone
	targets := ScanTombstone(nil, &self)
	targets = ScanTombstone(targets, &environment)
	err := q.QueryRowContext(ctx,
		`SELECT s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		        e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		        p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE s.id = $1`,
		serviceID,
	).Scan(ScanTombstone(targets, &project)...)
	if err != nil {
		return nil, err
	}
	return EffectiveDeletion(self, environment, project), nil
}

// requireLiveService rejects work on an effectively deleted service.
func requireLiveService(deletion *DeletionInfo) error {
	if deletion != nil {
		return ErrServiceDeleted
	}
	return nil
}

// tombstoneServiceTx marks the service deleted. It reports whether the row
// transitioned; an already-tombstoned row is a no-op success so concurrent
// deletes and retries stay idempotent.
func (s *persistence) tombstoneServiceTx(ctx context.Context, tx *sql.Tx, serviceID, userID string, now time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET deleted_at = $1,
		        deleted_by_user_id = $2,
		        delete_expires_at = $3
		  WHERE id = $4 AND deleted_at IS NULL`,
		now, userID, now.Add(s.deletionGrace), serviceID,
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

// hasLiveDomainBindingsQuerier reports whether the service has any bindings
// still routed. Tombstoned bindings are already withdrawn from ingress.
func (s *persistence) hasLiveDomainBindingsQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM domain_bindings d
			 JOIN services s ON s.id = d.service_id
			 JOIN environments e ON e.id = s.environment_id
			 JOIN projects p ON p.id = e.project_id
			WHERE d.service_id = $1
			  AND d.deleted_at IS NULL
			  AND s.deleted_at IS NULL
			  AND e.deleted_at IS NULL
			  AND p.deleted_at IS NULL
		)`,
		serviceID,
	).Scan(&exists)
	return exists, err
}
