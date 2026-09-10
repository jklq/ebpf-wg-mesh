package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/protobuf/encoding/protojson"
)

func (d *Delivery) EnsureManagedService(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec, trustedAgentID string) (ServiceRecord, []string, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	var rec ServiceRecord
	var affectedAgentIDs []string
	err := s.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		trustedAgentID = strings.TrimSpace(trustedAgentID)
		if trustedAgentID == "" {
			return errors.New("trusted agent id required for managed service")
		}
		var agentExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1)`, trustedAgentID).Scan(&agentExists); err != nil {
			return err
		}
		if !agentExists {
			return fmt.Errorf("%w: trusted agent %s is not enrolled", ErrNoPlacementAvailable, trustedAgentID)
		}
		environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
			WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
		if err != nil {
			return err
		}
		var hasOtherWorkloads bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM allocations a JOIN services s ON s.id = a.service_id
				 WHERE a.agent_id = $1
				   AND NOT (s.environment_id = $2 AND s.name = $3)
			)`,
			trustedAgentID, environment.ID, name,
		).Scan(&hasOtherWorkloads); err != nil {
			return err
		}
		if hasOtherWorkloads {
			return fmt.Errorf("%w: trusted agent %s is not dedicated to the managed dashboard", ErrNoPlacementAvailable, trustedAgentID)
		}
		current, found, err := s.serviceByNameQuerier(ctx, tx, environment.ID, name)
		if err != nil {
			return err
		}
		if !found {
			rec, err = d.createServiceTxInternal(ctx, tx, projectID, name, spec, trustedAgentID)
			affectedAgentIDs = []string{trustedAgentID}
			return err
		}
		spec = CanonicalServiceSpec(spec)
		placementChanged := current.AllocatedAgentID != trustedAgentID
		trustedAllocationExists := false
		if placementChanged {
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(
					SELECT 1 FROM allocations
					 WHERE service_id = $1 AND agent_id = $2
					   AND rollout_state IN ($3, $4)
				)`,
				current.ID, trustedAgentID, AllocationRolloutStarting, AllocationRolloutServing,
			).Scan(&trustedAllocationExists); err != nil {
				return err
			}
		}
		if sameServiceSpec(current.Spec, spec) && (!placementChanged || trustedAllocationExists) {
			rec = current
			if trustedAllocationExists {
				rec.AllocatedAgentID = trustedAgentID
			}
			return nil
		}
		now := time.Now().UTC()
		nextSpecRevision := current.SpecRevision + 1
		nextRolloutGeneration := current.RolloutGeneration + 1
		desiredReplicas := specReplicaCount(spec, current.DesiredReplicaCount)
		if err := validateDesiredReplicaCount(desiredReplicas); err != nil {
			return err
		}
		if err := validateVolumeReplicaCompatibility(spec, desiredReplicas); err != nil {
			return err
		}
		var existing []AllocationRecord
		if placementChanged {
			existing, err = s.listAllocationsByServiceIDQuerier(ctx, tx, current.ID, true)
			if err != nil {
				return err
			}
			rolloutService := current
			rolloutService.Spec = spec
			if _, err := d.prepareReplacementRolloutTx(ctx, tx, rolloutService, existing, now); err != nil {
				return err
			}
		}
		specJSON, err := protojson.Marshal(spec)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE services
				    SET current_spec_revision = $1,
				        current_rollout_generation = $2,
				        current_resolved_image = $3,
				        desired_replica_count = $4,
				        updated_at = $5
				  WHERE id = $6`,
			nextSpecRevision,
			nextRolloutGeneration,
			directImageRef(spec),
			desiredReplicas,
			now,
			current.ID,
		); err != nil {
			return err
		}
		journal.RecordService(ctx, current.ID)
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			current.ID,
			nextSpecRevision,
			specJSON,
			now,
		); err != nil {
			return err
		}
		journal.RecordRevision(ctx, current.ID, nextSpecRevision)
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", now); err != nil {
			return err
		}
		deployment, err := s.insertDeploymentTx(ctx, tx, current.ID, DeploymentStateScheduling, deploymentActor{Kind: DeploymentCauseSystem}, reasonManagedSync, "Managed service synchronized", nextSpecRevision, nextRolloutGeneration, "", directImageRef(spec), "", now)
		if err != nil {
			return err
		}
		rec = current
		rec.Spec = spec
		rec.SpecRevision = nextSpecRevision
		rec.RolloutGeneration = nextRolloutGeneration
		rec.AllocatedAgentID = trustedAgentID
		rec.ResolvedImage = directImageRef(spec)
		rec.DesiredReplicaCount = desiredReplicas
		rec.UpdatedAt = now
		if placementChanged {
			if _, err := d.insertAllocationTx(ctx, tx, rec, trustedAgentID, now); err != nil {
				return err
			}
			if _, err := d.advanceRolloutTx(ctx, tx, current.ID, now); err != nil {
				return err
			}
		} else {
			if err := d.retargetAllocationsTx(ctx, tx, current.ID, trustedAgentID,
				deployment.ID, nextSpecRevision, nextRolloutGeneration, now); err != nil {
				return err
			}
		}
		if source.DesiredSourceSpec(spec) != nil {
			if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
				return err
			}
		}
		affectedAgentIDs = appendAllocationAgentIDs([]string{trustedAgentID}, existing...)
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	if len(affectedAgentIDs) == 0 {
		return rec, nil, nil
	}
	allAgentIDs, err := s.agentIDs(ctx)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return rec, allAgentIDs, nil
}
