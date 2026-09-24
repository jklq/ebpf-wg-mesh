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
	// Registry I/O happens outside the product transaction and outside
	// the scheduler lock, and only when the spec's image input is about to
	// deploy: an unchanged spec keeps its stored artifact and
	// reconciliation never blocks on the registry or stalls other
	// scheduler-serialized mutations. A spec racing the pre-read retries
	// with a fresh resolution.
	var rec ServiceRecord
	var affectedAgentIDs []string
	var errEnsure error
	for attempt := 0; attempt < 3; attempt++ {
		pre, err := d.preResolveManagedImage(ctx, projectID, name, spec)
		if err != nil {
			return ServiceRecord{}, nil, err
		}
		rec, affectedAgentIDs, errEnsure = d.ensureManagedServiceTx(ctx, projectID, name, spec, trustedAgentID, pre)
		if !errors.Is(errEnsure, errDirectImageChanged) {
			break
		}
	}
	if errEnsure != nil {
		return ServiceRecord{}, nil, errEnsure
	}
	if len(affectedAgentIDs) == 0 {
		return rec, nil, nil
	}
	allAgentIDs, err := d.store.agentIDs(ctx)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return rec, allAgentIDs, nil
}

func (d *Delivery) ensureManagedServiceTx(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec, trustedAgentID string, pre resolvedDirectImage) (ServiceRecord, []string, error) {
	// The scheduler lock serializes the mutation phase only; the managed
	// pre-read and registry resolution run outside it (see
	// EnsureManagedService).
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	var rec ServiceRecord
	var affectedAgentIDs []string
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
			rec, err = d.createServiceTxInternal(ctx, tx, projectID, name, spec, trustedAgentID, pre)
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
		artifactID := current.ResolvedArtifactID
		// A changed spec resolves the tag at deploy time. An unchanged spec
		// — a placement-only migration — keeps the stored artifact so a moved
		// tag can never swap the image under it.
		if image := strings.TrimSpace(directImageRef(spec)); image != "" && (!sameServiceSpec(current.Spec, spec) || artifactID == "") {
			artifact, err := d.directImageArtifactTx(ctx, tx, current.ID, image, pre, deploymentActor{Kind: DeploymentCauseSystem}, now)
			if err != nil {
				return err
			}
			artifactID = artifact.ID
		}
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
				        desired_replica_count = $2,
				        updated_at = $3
				  WHERE id = $4`,
			nextSpecRevision,
			desiredReplicas,
			now,
			current.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE service_delivery_status
			SET current_rollout_generation = $1, current_artifact_id = NULLIF($2, ''), updated_at = $3
			WHERE service_id = $4`, nextRolloutGeneration, artifactID, now, current.ID); err != nil {
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
		deployment, err := s.insertDeploymentTx(ctx, tx, current.ID, DeploymentStateScheduling, deploymentActor{Kind: DeploymentCauseSystem}, reasonManagedSync, "Managed service synchronized", nextSpecRevision, nextRolloutGeneration, "", artifactID, "", now)
		if err != nil {
			return err
		}
		rec = current
		rec.Spec = spec
		rec.SpecRevision = nextSpecRevision
		rec.RolloutGeneration = nextRolloutGeneration
		rec.AllocatedAgentID = trustedAgentID
		rec.ResolvedArtifactID = artifactID
		rec.ResolvedImage = deployment.ImageDigest
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
	return rec, affectedAgentIDs, err
}

// managedServiceByName pre-reads the managed service without locks so
// registry I/O can be skipped when nothing deploys.
func (s *persistence) managedServiceByName(ctx context.Context, projectID, name string) (ServiceRecord, bool, error) {
	environment, err := scanEnvironmentRow(s.db.QueryRowContext(ctx, environmentSelect+` WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRecord{}, false, nil
	}
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return s.serviceByNameQuerier(ctx, s.db, environment.ID, name)
}
