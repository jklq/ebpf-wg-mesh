package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func (d *Delivery) CreateScheduledService(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createScheduledService(ctx, scope, name, spec)
}

func (d *Delivery) createScheduledService(ctx context.Context, scope authz.Environment, name string, spec *platformv1.ServiceSpec) (ServiceRecord, error) {
	s := d.store
	var rec ServiceRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		environment, err := s.environmentByIDQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if environment.Deletion != nil {
			return ErrEnvironmentDeleted
		}
		rec, err = s.createStagedServiceTx(ctx, tx, environment, name, spec, scope.UserID())
		return err
	})
	if err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) CreateService(ctx context.Context, user authz.User, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Write)
	if err != nil {
		return ServiceRecord{}, err
	}
	// Registry resolution runs outside the scheduler lock like every pre-resolution: a slow
	// registry must not stall unrelated serialized mutations (see ReleaseEnvironment).
	pre, err := d.preResolveDirectImage(ctx, spec)
	if err != nil {
		return ServiceRecord{}, err
	}
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	var rec ServiceRecord
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = d.createServiceTx(ctx, tx, scope, name, spec, agentID, pre)
		return err
	})
	if err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) createServiceTx(ctx context.Context, tx *sql.Tx, scope authz.Environment, name string, spec *platformv1.ServiceSpec, agentID string, pre resolvedDirectImage) (ServiceRecord, error) {
	environment, err := d.store.environmentByIDQuerier(ctx, tx, scope)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, scope.UserID(), pre)
}

func (d *Delivery) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string, pre resolvedDirectImage) (ServiceRecord, error) {
	s := d.store
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return ServiceRecord{}, err
	}
	environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, "system", pre)
}

func (d *Delivery) createDeployedServiceTx(ctx context.Context, tx *sql.Tx, environment EnvironmentRecord, name string, spec *platformv1.ServiceSpec, agentID, actorUserID string, pre resolvedDirectImage) (ServiceRecord, error) {
	s := d.store
	if environment.Deletion != nil {
		return ServiceRecord{}, ErrEnvironmentDeleted
	}
	if agentID == "" {
		return ServiceRecord{}, errors.New("agent id required")
	}
	if volumeName := ServiceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, environment.ID, volumeName); err != nil {
			return ServiceRecord{}, err
		}
	}
	if spec == nil {
		spec = &platformv1.ServiceSpec{}
	}
	if !specHasDesiredReplicaCount(spec) {
		spec.DesiredReplicaCount = replicaCountPtr(DefaultDesiredReplicaCount)
	}
	if err := validateDesiredReplicaCount(spec.GetDesiredReplicaCount()); err != nil {
		return ServiceRecord{}, err
	}
	if err := validateVolumeReplicaCompatibility(spec, spec.GetDesiredReplicaCount()); err != nil {
		return ServiceRecord{}, err
	}
	rec, err := s.insertServiceTx(ctx, tx, environment, name, spec, agentID, actorUserID)
	if err != nil {
		return ServiceRecord{}, err
	}
	now := rec.CreatedAt
	actor := deploymentActor{Kind: DeploymentCauseSystem}
	if strings.TrimSpace(actorUserID) != "" && actorUserID != "system" {
		actor = deploymentActor{Kind: DeploymentCauseUser, ID: actorUserID}
	}
	if image := directImageRef(rec.Spec); image != "" {
		artifact, err := d.directImageArtifactTx(ctx, tx, rec.ID, image, pre, actor, now)
		if err != nil {
			return ServiceRecord{}, err
		}
		rec.ResolvedArtifactID = artifact.ID
		rec.ResolvedImage = artifact.ImageRef
	}
	rec.RolloutGeneration = 1
	if _, err := tx.ExecContext(ctx, `UPDATE service_delivery_status
		SET current_rollout_generation = 1, current_artifact_id = NULLIF($1, '') WHERE service_id = $2`, rec.ResolvedArtifactID, rec.ID); err != nil {
		return ServiceRecord{}, err
	}
	journal.RecordService(ctx, rec.ID)
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, 1, 1, "create", "", "", now); err != nil {
		return ServiceRecord{}, err
	}
	initialState := DeploymentStateScheduling
	reasonCode := reasonServiceCreated
	detail := "Service created and scheduled"
	if source.DesiredSourceSpec(spec) != nil {
		initialState = DeploymentStateStaged
		reasonCode = reasonServiceStaged
		detail = "Service created; waiting for source build"
	}
	dep, err := s.insertDeploymentTx(ctx, tx, rec.ID, initialState, deploymentActor{Kind: DeploymentCauseSystem}, reasonCode, detail, rec.SpecRevision, 1, "", rec.ResolvedArtifactID, "", now)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.LatestDeployment = &dep
	rec.DesiredReplicaCount = specReplicaCount(spec, DefaultDesiredReplicaCount)
	if _, err := d.reconcileServiceReplicasTx(ctx, tx, rec, agentID, now); err != nil {
		return ServiceRecord{}, err
	}
	if source.DesiredSourceSpec(spec) != nil {
		if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
			return ServiceRecord{}, err
		}
	}
	return rec, nil
}

func (d *Delivery) UpdateService(ctx context.Context, user authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return d.updateService(ctx, scope, name, spec)
}

func (d *Delivery) updateService(ctx context.Context, scope authz.Service, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, error) {
	s := d.store
	var current ServiceRecord
	var changed bool
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		current, changed, _, err = d.updateServiceTx(ctx, tx, scope, name, spec)
		return err
	})
	if err != nil {
		return ServiceRecord{}, false, err
	}
	current, err = s.serviceByID(ctx, scope)
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return current, changed, nil
}

func (d *Delivery) updateServiceTx(ctx context.Context, tx *sql.Tx, scope authz.Service, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, bool, error) {
	s := d.store
	current, err := s.serviceByIDQuerier(ctx, tx, scope)
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	// Lock and re-check: the revision guard cannot see a concurrent tombstone.
	deletion, err := s.lockServiceDeletionTx(ctx, tx, current.ID)
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	if err := requireLiveService(deletion); err != nil {
		return ServiceRecord{}, false, false, err
	}
	nextName := strings.TrimSpace(name)
	if nextName == "" {
		nextName = current.Name
	}
	if volumeName := ServiceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, current.EnvironmentID, volumeName); err != nil {
			return ServiceRecord{}, false, false, err
		}
	}
	pendingReplicas := specReplicaCount(spec, current.DesiredReplicaCount)
	if live := current.DesiredReplicaCount; live > pendingReplicas {
		pendingReplicas = live
	}
	if err := validateVolumeReplicaCompatibility(spec, pendingReplicas); err != nil {
		return ServiceRecord{}, false, false, err
	}
	spec = CanonicalServiceSpec(spec)
	if err := ValidateServicePlacement(spec); err != nil {
		return ServiceRecord{}, false, false, err
	}
	if err := ValidateBuildRecipe(spec); err != nil {
		return ServiceRecord{}, false, false, err
	}
	if err := ValidateRollingStrategy(spec); err != nil {
		return ServiceRecord{}, false, false, err
	}
	if err := d.rejectSealedNameConflicts(ctx, tx, scope.ID(), spec.GetRuntime().GetEnv()); err != nil {
		return ServiceRecord{}, false, false, err
	}
	nameChanged := nextName != current.Name
	if sameServiceSpec(current.Spec, spec) {
		if !nameChanged {
			return current, false, false, nil
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET name = $1,
			        updated_at = $2
			  WHERE id = $3
			    AND current_spec_revision = $4
			    AND EXISTS (SELECT 1 FROM service_delivery_status ds WHERE ds.service_id = services.id AND COALESCE(ds.current_rollout_generation, 0) = $5)`,
			nextName, now, current.ID, current.SpecRevision, current.RolloutGeneration,
		)
		if err != nil {
			return ServiceRecord{}, false, false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return ServiceRecord{}, false, false, err
		}
		if affected == 0 {
			return ServiceRecord{}, false, false, ErrConcurrentUpdate
		}
		journal.RecordService(ctx, current.ID)
		current.Name = nextName
		current.UpdatedAt = now
		return current, false, false, nil
	}

	now := time.Now().UTC()
	nextSpecRevision := current.SpecRevision + 1
	sourceChanged := source.DesiredSourceSpec(spec) != nil && !sameDesiredSourceSpec(current.Spec, spec)
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE services
		    SET name = $1,
		        current_spec_revision = $2,
		        updated_at = $3
		  WHERE id = $4
		    AND current_spec_revision = $5
		    AND EXISTS (SELECT 1 FROM service_delivery_status ds WHERE ds.service_id = services.id AND COALESCE(ds.current_rollout_generation, 0) = $6)`,
		nextName, nextSpecRevision, now, current.ID, current.SpecRevision, current.RolloutGeneration,
	)
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	if affected == 0 {
		return ServiceRecord{}, false, false, ErrConcurrentUpdate
	}
	journal.RecordService(ctx, current.ID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		current.ID, nextSpecRevision, specJSON, now,
	); err != nil {
		return ServiceRecord{}, false, false, err
	}
	journal.RecordRevision(ctx, current.ID, nextSpecRevision)
	nextRecord := current
	nextRecord.Name = nextName
	nextRecord.Spec = spec
	if desired := source.DesiredSourceSpec(spec); desired != nil {
		nextRecord.SourceSummary = toProtoSourceStateSummary(desired, nil, nil, nil)
	} else {
		nextRecord.SourceSummary = BuildSourceSummary(spec)
	}
	nextRecord.SpecRevision = nextSpecRevision
	nextRecord.UpdatedAt = now
	nextRecord.PendingChanges = true
	return nextRecord, true, sourceChanged, nil
}

func (d *Delivery) DeleteService(ctx context.Context, user authz.User, serviceID string) error {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return err
	}
	return d.deleteService(ctx, scope)
}

func (d *Delivery) deleteService(ctx context.Context, scope authz.Service) error {
	s := d.store
	var hasBindings bool
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Lock first: serialize against concurrent deploys.
		deletion, err := s.lockServiceDeletionTx(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		if deletion != nil && !deletion.Inherited {
			return nil
		}
		// Capture before tombstoning: the live-binding check needs a live row.
		bindings, err := s.hasLiveDomainBindingsQuerier(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		hasBindings = bindings
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		tombstoned, err := s.tombstoneServiceTx(ctx, tx, scope.ID(), scope.UserID(), now)
		if err != nil {
			return err
		}
		if !tombstoned {
			return nil
		}
		if err := quiesceServiceTx(ctx, s, tx, scope.ID(), scope.UserID()); err != nil {
			return err
		}
		// Record before dropping assignments: the commit resolves their absence as removals.
		if err := journal.RecordServiceRemoval(ctx, tx, scope.ID()); err != nil {
			return err
		}
		// Assignments are placement records, not recoverable data: drop them so durable rows
		// match the live view. Restore re-asserts the empty set.
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_assignments WHERE service_id = $1`, scope.ID()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if d.notifier != nil {
		ids, listErr := s.agentIDs(ctx)
		if listErr != nil {
			return listErr
		}
		for _, agentID := range ids {
			d.notifier.Notify(agentID)
		}
	}
	if hasBindings && d.ingress != nil {
		d.ingress.RequestSync()
	}
	return nil
}

// RestoreService clears a service's own tombstone within the grace period. History survives;
// a release resumes work since assignments dropped at delete. Restore top-down only.
func (d *Delivery) RestoreService(ctx context.Context, user authz.User, serviceID string) (ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.restoreService(ctx, scope)
}

func (d *Delivery) restoreService(ctx context.Context, scope authz.Service) (ServiceRecord, error) {
	s := d.store
	var rec ServiceRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		deletion, err := s.lockServiceDeletionTx(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		if deletion == nil {
			rec, err = s.serviceByIDQuerier(ctx, tx, scope)
			return err
		}
		if deletion.Inherited {
			return ErrAncestorDeleted
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET deleted_at = NULL,
			        deleted_by_user_id = '',
			        delete_expires_at = NULL
			  WHERE id = $1 AND deleted_at IS NOT NULL AND delete_expires_at > statement_timestamp()`,
			scope.ID(),
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrDeletionExpired
		}
		// Re-assert the empty set so a restore never resurrects stale placements.
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocation_assignments WHERE service_id = $1`, scope.ID()); err != nil {
			return err
		}
		if err := journal.RecordServiceRemoval(ctx, tx, scope.ID()); err != nil {
			return err
		}
		rec, err = s.serviceByIDQuerier(ctx, tx, scope)
		return err
	})
	if err != nil {
		return ServiceRecord{}, err
	}
	if d.notifier != nil {
		ids, listErr := s.agentIDs(ctx)
		if listErr != nil {
			return ServiceRecord{}, listErr
		}
		for _, agentID := range ids {
			d.notifier.Notify(agentID)
		}
	}
	if d.ingress != nil {
		d.ingress.RequestSync()
	}
	return rec, nil
}

func (d *Delivery) DiscardServiceChanges(ctx context.Context, user authz.User, serviceID string, changeIDs []string, discardAll bool) (ServiceRecord, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.discardServiceChanges(ctx, scope, changeIDs, discardAll)
}

func (d *Delivery) discardServiceChanges(ctx context.Context, scope authz.Service, changeIDs []string, discardAll bool) (ServiceRecord, error) {
	s := d.store
	var rec ServiceRecord
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.serviceByIDQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		deletion, err := s.lockServiceDeletionTx(ctx, tx, current.ID)
		if err != nil {
			return err
		}
		if err := requireLiveService(deletion); err != nil {
			return err
		}
		changes, deployed, err := s.loadServiceUnappliedChangesQuerier(ctx, tx, current.ID, current.Spec, current.RolloutGeneration)
		if err != nil {
			return err
		}
		valid := make(map[string]struct{}, len(changes))
		for _, change := range changes {
			valid[change.GetId()] = struct{}{}
		}
		filtered := make([]string, 0, len(changeIDs))
		for _, id := range changeIDs {
			if _, ok := valid[id]; ok {
				filtered = append(filtered, id)
			}
		}
		if !discardAll && len(filtered) == 0 {
			rec = current
			return nil
		}
		nextSpec := applyDiscardedServiceChanges(current.Spec, deployed, discardAll, filtered)
		if sameServiceSpec(current.Spec, nextSpec) {
			rec = current
			return nil
		}
		now := time.Now().UTC()
		nextRevision := current.SpecRevision + 1
		specJSON, err := protojson.Marshal(nextSpec)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET current_spec_revision = $1,
			        updated_at = $2
			  WHERE id = $3
			    AND current_spec_revision = $4
			    AND EXISTS (SELECT 1 FROM service_delivery_status ds WHERE ds.service_id = services.id AND COALESCE(ds.current_rollout_generation, 0) = $5)`,
			nextRevision, now, current.ID, current.SpecRevision, current.RolloutGeneration,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrConcurrentUpdate
		}
		journal.RecordService(ctx, current.ID)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			current.ID, nextRevision, specJSON, now,
		); err != nil {
			return err
		}
		journal.RecordRevision(ctx, current.ID, nextRevision)
		rec = current
		rec.Spec = nextSpec
		rec.SpecRevision = nextRevision
		rec.UpdatedAt = now
		if desired := source.DesiredSourceSpec(nextSpec); desired != nil {
			rec.SourceSummary = toProtoSourceStateSummary(desired, nil, nil, nil)
		} else {
			rec.SourceSummary = BuildSourceSummary(nextSpec)
		}
		return nil
	})
	if err != nil {
		return ServiceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	rec, err = s.serviceByID(ctx, scope)
	if err != nil {
		return ServiceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	return rec, nil
}

func (d *Delivery) ScaleService(ctx context.Context, user authz.User, serviceID string, desired int32) (ServiceRecord, []AllocationRecord, int64, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return ServiceRecord{}, nil, 0, err
	}
	service, allocations, err := d.scaleService(ctx, scope, desired)
	if err != nil {
		return ServiceRecord{}, nil, 0, err
	}
	var eventIndex int64
	if d.events != nil {
		eventIndex, err = d.events.Current(ctx)
		if err != nil {
			return ServiceRecord{}, nil, 0, err
		}
	}
	return service, allocations, eventIndex, nil
}

func (d *Delivery) scaleService(ctx context.Context, scope authz.Service, desired int32) (ServiceRecord, []AllocationRecord, error) {
	s := d.store
	var (
		current     ServiceRecord
		allocations []AllocationRecord
	)
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		current, allocations, err = d.scaleServiceTx(ctx, tx, scope, desired)
		return err
	})
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	current, err = s.serviceByID(ctx, scope)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocations, err = s.listAllocationsByServiceID(ctx, scope.ID())
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return current, allocations, nil
}

func (d *Delivery) scaleServiceTx(ctx context.Context, tx *sql.Tx, scope authz.Service, desired int32) (ServiceRecord, []AllocationRecord, error) {
	s := d.store
	if err := validateDesiredReplicaCount(desired); err != nil {
		return ServiceRecord{}, nil, err
	}
	current, err := s.serviceByIDQuerier(ctx, tx, scope)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	serviceID := scope.ID()
	deletion, err := s.lockServiceDeletionTx(ctx, tx, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	if err := requireLiveService(deletion); err != nil {
		return ServiceRecord{}, nil, err
	}
	if err := validateVolumeReplicaCompatibility(current.Spec, desired); err != nil {
		return ServiceRecord{}, nil, err
	}

	nextSpec := current.Spec
	if nextSpec == nil {
		nextSpec = &platformv1.ServiceSpec{}
	} else {
		nextSpec = proto.Clone(nextSpec).(*platformv1.ServiceSpec)
	}
	nextSpec.DesiredReplicaCount = replicaCountPtr(desired)
	updated, _, _, err := d.updateServiceTx(ctx, tx, scope, current.Name, nextSpec)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, false)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return updated, allocations, nil
}

func (s *persistence) createStagedServiceTx(ctx context.Context, tx *sql.Tx, environment EnvironmentRecord, name string, spec *platformv1.ServiceSpec, actorUserID string) (ServiceRecord, error) {
	rec, err := s.insertServiceTx(ctx, tx, environment, name, spec, "", actorUserID)
	if err != nil {
		return ServiceRecord{}, err
	}
	dep, err := s.insertDeploymentTx(ctx, tx, rec.ID, DeploymentStateStaged, deploymentActor{Kind: DeploymentCauseUser}, reasonServiceStaged, "Configuration staged", rec.SpecRevision, 0, "", "", "", rec.CreatedAt)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.LatestDeployment = &dep
	return rec, nil
}

func (s *persistence) insertServiceTx(ctx context.Context, tx *sql.Tx, environment EnvironmentRecord, name string, spec *platformv1.ServiceSpec, agentID, actorUserID string) (ServiceRecord, error) {
	now := time.Now().UTC()
	spec = CanonicalServiceSpec(spec)
	if spec == nil {
		spec = &platformv1.ServiceSpec{}
	}
	if err := ValidateServicePlacement(spec); err != nil {
		return ServiceRecord{}, err
	}
	if err := ValidateBuildRecipe(spec); err != nil {
		return ServiceRecord{}, err
	}
	if !specHasDesiredReplicaCount(spec) {
		spec.DesiredReplicaCount = replicaCountPtr(DefaultDesiredReplicaCount)
	}
	if err := ValidateRollingStrategy(spec); err != nil {
		return ServiceRecord{}, err
	}
	rec := ServiceRecord{
		ID:                  uuid.NewString(),
		EnvironmentID:       environment.ID,
		ProjectID:           environment.ProjectID,
		Name:                strings.TrimSpace(name),
		Spec:                spec,
		SpecRevision:        1,
		AllocatedAgentID:    agentID,
		DesiredReplicaCount: specReplicaCount(spec, DefaultDesiredReplicaCount),
		CreatedAt:           now,
		UpdatedAt:           now,
		PendingChanges:      true,
	}
	if desired := source.DesiredSourceSpec(spec); desired != nil {
		rec.SourceSummary = toProtoSourceStateSummary(desired, nil, nil, nil)
	} else {
		rec.SourceSummary = BuildSourceSummary(spec)
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return ServiceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(
			id, environment_id, name, current_spec_revision,
			desired_replica_count, created_at, updated_at
		) VALUES ($1, $2, $3, 1, $4, $5, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.DesiredReplicaCount, now,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ServiceRecord{}, s.serviceNameConflictErr(ctx, tx, environment.ID, rec.Name)
		}
		return ServiceRecord{}, err
	}
	journal.RecordService(ctx, rec.ID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return ServiceRecord{}, err
	}
	journal.RecordRevision(ctx, rec.ID, rec.SpecRevision)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_delivery_status(service_id, updated_at) VALUES ($1, $2)`,
		rec.ID, now,
	); err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}

// serviceNameConflictErr maps a service-name collision to a typed error.
// Tombstoned rows reserve their names for the grace period; the error tells
// the caller to restore or wait instead.
func (s *persistence) serviceNameConflictErr(ctx context.Context, tx *sql.Tx, environmentID, name string) error {
	existing, found, err := s.serviceByNameQuerier(ctx, tx, environmentID, name)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("service %q already exists", name)
		}
		return fmt.Errorf("%w: %v", ErrServiceAlreadyExists, err)
	}
	if existing.Deletion != nil && !existing.Deletion.Inherited {
		return fmt.Errorf("%w: %q was deleted; restore it or wait until %s", ErrServiceAlreadyExists, name, existing.Deletion.ExpiresAt.Format(time.RFC3339))
	}
	return fmt.Errorf("%w: %q", ErrServiceAlreadyExists, name)
}

// CheckDeletionConfirmation requires a typed confirmation exactly matching the current name.
func CheckDeletionConfirmation(current, confirmation string) error {
	if current == "" || strings.TrimSpace(confirmation) != current {
		return ErrConfirmationMismatch
	}
	return nil
}

// ServiceQuiescer performs service-level deletion quiesce inside a caller's product
// transaction, letting catalog operations reuse delivery's transitions.
type ServiceQuiescer struct {
	store *persistence
}

// NewServiceQuiescer builds a quiescer bound to the caller's transaction.
func NewServiceQuiescer(sourceStore SourceStore) *ServiceQuiescer {
	return &ServiceQuiescer{store: &persistence{sourceStore: sourceStore, deletionGrace: DefaultDeletionGracePeriod}}
}

// QuiesceTx stops new work for one service: its deployment moves to Removed, queued builds
// cancel, and pending source work drops. Running builds finish into the terminal deployment.
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

// lockServiceDeletionTx locks the service row and reports its effective deletion state.
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

// serviceDeletionQuerier reports effective deletion state without locking. Mutating callers
// must lock first (lockServiceDeletionTx) and re-check.
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

// tombstoneServiceTx marks the service deleted, reporting whether the row transitioned. An
// already-tombstoned row is a no-op success so deletes stay idempotent.
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

// hasLiveDomainBindingsQuerier reports whether the service has bindings still routed.
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
