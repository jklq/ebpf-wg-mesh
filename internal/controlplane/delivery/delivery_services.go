package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func (d *Delivery) CreateScheduledService(ctx context.Context, environmentID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createScheduledService(ctx, identity.UserID, environmentID, name, spec)
}

func (d *Delivery) createScheduledService(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, error) {
	s := d.store
	var rec ServiceRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		environment, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
		if err != nil {
			return err
		}
		rec, err = s.createStagedServiceTx(ctx, tx, environment, name, spec, userID)
		return err
	})
	if err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) CreateService(ctx context.Context, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (ServiceRecord, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	var rec ServiceRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = d.createServiceTx(ctx, tx, userID, environmentID, name, spec, agentID)
		return err
	})
	if err != nil {
		return ServiceRecord{}, err
	}
	return rec, nil
}

func (d *Delivery) createServiceTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (ServiceRecord, error) {
	environment, err := d.store.authorizeEnvironmentWriteQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, userID)
}

func (d *Delivery) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (ServiceRecord, error) {
	s := d.store
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return ServiceRecord{}, err
	}
	environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, "system")
}

func (d *Delivery) createDeployedServiceTx(ctx context.Context, tx *sql.Tx, environment EnvironmentRecord, name string, spec *platformv1.ServiceSpec, agentID, actorUserID string) (ServiceRecord, error) {
	s := d.store
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
	rec.RolloutGeneration = 1
	rec.ResolvedImage = directImageRef(spec)
	if _, err := tx.ExecContext(ctx, `UPDATE services
		SET current_rollout_generation = 1, current_resolved_image = $1 WHERE id = $2`, rec.ResolvedImage, rec.ID); err != nil {
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
	dep, err := s.insertDeploymentTx(ctx, tx, rec.ID, initialState, deploymentActor{Kind: DeploymentCauseSystem}, reasonCode, detail, rec.SpecRevision, 1, "", rec.ResolvedImage, "", now)
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

func (d *Delivery) UpdateService(ctx context.Context, serviceID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return d.updateService(ctx, identity.UserID, serviceID, name, spec)
}

func (d *Delivery) updateService(ctx context.Context, userID, serviceID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, error) {
	s := d.store
	var current ServiceRecord
	var changed bool
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		current, changed, _, err = d.updateServiceTx(ctx, tx, userID, serviceID, name, spec)
		return err
	})
	if err != nil {
		return ServiceRecord{}, false, err
	}
	current, err = s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return current, changed, nil
}

func (d *Delivery) updateServiceTx(ctx context.Context, tx *sql.Tx, userID, serviceID, name string, spec *platformv1.ServiceSpec) (ServiceRecord, bool, bool, error) {
	s := d.store
	current, err := s.serviceByIDQuerier(ctx, tx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, false, false, err
	}
	if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
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
	if err := ValidateRollingStrategy(spec); err != nil {
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
			    AND current_rollout_generation = $5`,
			nextName, now, serviceID, current.SpecRevision, current.RolloutGeneration,
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
		journal.RecordService(ctx, serviceID)
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
		    AND current_rollout_generation = $6`,
		nextName, nextSpecRevision, now, serviceID, current.SpecRevision, current.RolloutGeneration,
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
	journal.RecordService(ctx, serviceID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		serviceID, nextSpecRevision, specJSON, now,
	); err != nil {
		return ServiceRecord{}, false, false, err
	}
	journal.RecordRevision(ctx, serviceID, nextSpecRevision)
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

func (d *Delivery) DeleteService(ctx context.Context, serviceID string) error {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return err
	}
	return d.deleteService(ctx, identity.UserID, serviceID)
}

func (d *Delivery) deleteService(ctx context.Context, userID, serviceID string) error {
	s := d.store
	var hasBindings bool
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		service, err := s.serviceByIDQuerier(ctx, tx, userID, serviceID)
		if err != nil {
			return err
		}
		if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, service.EnvironmentID); err != nil {
			return err
		}
		bindings, err := s.listDomainBindings(ctx, userID, serviceID)
		if err != nil {
			return err
		}
		hasBindings = len(bindings) > 0
		if err := s.markCurrentDeploymentRemovedTx(ctx, tx, serviceID, deploymentActor{Kind: DeploymentCauseUser, ID: userID}); err != nil {
			return err
		}
		if err := journal.RecordServiceRemoval(ctx, tx, serviceID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id = $1`, serviceID)
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

func (d *Delivery) DiscardServiceChanges(ctx context.Context, serviceID string, changeIDs []string, discardAll bool) (ServiceRecord, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return ServiceRecord{}, err
	}
	return d.discardServiceChanges(ctx, identity.UserID, serviceID, changeIDs, discardAll)
}

func (d *Delivery) discardServiceChanges(ctx context.Context, userID, serviceID string, changeIDs []string, discardAll bool) (ServiceRecord, error) {
	s := d.store
	var rec ServiceRecord
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := s.serviceByIDQuerier(ctx, tx, userID, serviceID)
		if err != nil {
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
			    AND current_rollout_generation = $5`,
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
		rec.ResolvedImage = directImageRef(nextSpec)
		return nil
	})
	if err != nil {
		return ServiceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	rec, err = s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	return rec, nil
}

func (d *Delivery) ScaleService(ctx context.Context, serviceID string, desired int32) (ServiceRecord, []AllocationRecord, int64, error) {
	identity, err := d.userFromContext(ctx)
	if err != nil {
		return ServiceRecord{}, nil, 0, err
	}
	service, allocations, err := d.scaleService(ctx, identity.UserID, serviceID, desired)
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

func (d *Delivery) scaleService(ctx context.Context, userID, serviceID string, desired int32) (ServiceRecord, []AllocationRecord, error) {
	s := d.store
	var (
		current     ServiceRecord
		allocations []AllocationRecord
	)
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		current, allocations, err = d.scaleServiceTx(ctx, tx, userID, serviceID, desired)
		return err
	})
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	current, err = s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocations, err = s.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return current, allocations, nil
}

func (d *Delivery) scaleServiceTx(ctx context.Context, tx *sql.Tx, userID, serviceID string, desired int32) (ServiceRecord, []AllocationRecord, error) {
	s := d.store
	if err := validateDesiredReplicaCount(desired); err != nil {
		return ServiceRecord{}, nil, err
	}
	current, err := s.serviceByIDQuerier(ctx, tx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	if _, err := s.authorizeEnvironmentWriteQuerier(ctx, tx, userID, current.EnvironmentID); err != nil {
		return ServiceRecord{}, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 FOR UPDATE`, serviceID).Scan(&serviceID); err != nil {
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
	updated, _, _, err := d.updateServiceTx(ctx, tx, userID, serviceID, current.Name, nextSpec)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, false)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return updated, allocations, nil
}
