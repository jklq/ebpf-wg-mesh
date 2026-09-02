package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) createVolumeTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, sizeBytes int64) (volumeRecord, error) {
	environment, err := s.environmentByIDQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return volumeRecord{}, err
	}
	rec := volumeRecord{
		ID:            mustID(),
		EnvironmentID: environment.ID,
		Name:          name,
		SizeBytes:     sizeBytes,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, environment_id, name, size_bytes, created_at) VALUES ($1, $2, $3, $4, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.SizeBytes, rec.CreatedAt,
	); err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createServiceTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	environment, err := s.environmentByIDQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return serviceRecord{}, err
	}
	return s.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, userID)
}

func (s *Store) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return serviceRecord{}, err
	}
	environment, err := scanEnvironmentRow(tx.QueryRowContext(ctx, environmentSelect+`
		WHERE e.project_id = $1 AND e.is_production = TRUE`, projectID))
	if err != nil {
		return serviceRecord{}, err
	}
	return s.createDeployedServiceTx(ctx, tx, environment, name, spec, agentID, "system")
}

func (s *Store) createStagedServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec, actorUserID string) (serviceRecord, error) {
	rec, err := s.insertServiceTx(ctx, tx, environment, name, spec, "", actorUserID)
	if err != nil {
		return serviceRecord{}, err
	}
	dep, err := s.insertDeploymentTx(ctx, tx, rec.ID, deploymentStateStaged, deploymentActor{Kind: deploymentCauseUser}, reasonServiceStaged, "Configuration staged", rec.SpecRevision, 0, "", "", "", rec.CreatedAt)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.LatestDeployment = &dep
	return rec, nil
}

func (s *Store) createDeployedServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec, agentID, actorUserID string) (serviceRecord, error) {
	if agentID == "" {
		return serviceRecord{}, errors.New("agent id required")
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, environment.ID, volumeName); err != nil {
			return serviceRecord{}, err
		}
	}
	if spec == nil {
		spec = &platformv1.ServiceSpec{}
	}
	if !specHasDesiredReplicaCount(spec) {
		spec.DesiredReplicaCount = replicaCountPtr(defaultDesiredReplicaCount)
	}
	if err := validateDesiredReplicaCount(spec.GetDesiredReplicaCount()); err != nil {
		return serviceRecord{}, err
	}
	if err := validateVolumeReplicaCompatibility(spec, spec.GetDesiredReplicaCount()); err != nil {
		return serviceRecord{}, err
	}
	rec, err := s.insertServiceTx(ctx, tx, environment, name, spec, agentID, actorUserID)
	if err != nil {
		return serviceRecord{}, err
	}
	now := rec.CreatedAt
	rec.RolloutGeneration = 1
	rec.ResolvedImage = directImageRef(spec)
	if _, err := tx.ExecContext(ctx, `UPDATE services
		SET current_rollout_generation = 1, current_resolved_image = $1 WHERE id = $2`, rec.ResolvedImage, rec.ID); err != nil {
		return serviceRecord{}, err
	}
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, 1, 1, "create", "", "", now); err != nil {
		return serviceRecord{}, err
	}
	initialState := deploymentStateScheduling
	reasonCode := reasonServiceCreated
	detail := "Service created and scheduled"
	if desiredSourceSpec(spec) != nil {
		initialState = deploymentStateStaged
		reasonCode = reasonServiceStaged
		detail = "Service created; waiting for source build"
	}
	dep, err := s.insertDeploymentTx(ctx, tx, rec.ID, initialState, deploymentActor{Kind: deploymentCauseSystem}, reasonCode, detail, rec.SpecRevision, 1, "", rec.ResolvedImage, "", now)
	if err != nil {
		return serviceRecord{}, err
	}
	rec.LatestDeployment = &dep
	rec.DesiredReplicaCount = specReplicaCount(spec, defaultDesiredReplicaCount)
	if _, err := s.reconcileServiceReplicasTx(ctx, tx, rec, agentID, now); err != nil {
		return serviceRecord{}, err
	}
	if desiredSourceSpec(spec) != nil {
		if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
			return serviceRecord{}, err
		}
	}
	// Allocations are part of every node's workload identity catalog. Creating
	// one therefore changes mesh policy globally even though only one agent runs
	// the workload.
	if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) insertServiceTx(ctx context.Context, tx *sql.Tx, environment environmentRecord, name string, spec *platformv1.ServiceSpec, agentID, actorUserID string) (serviceRecord, error) {
	now := time.Now().UTC()
	spec = canonicalServiceSpec(spec)
	if spec == nil {
		spec = &platformv1.ServiceSpec{}
	}
	if err := validateServicePlacement(spec); err != nil {
		return serviceRecord{}, err
	}
	if !specHasDesiredReplicaCount(spec) {
		spec.DesiredReplicaCount = replicaCountPtr(defaultDesiredReplicaCount)
	}
	if err := validateRollingStrategy(spec); err != nil {
		return serviceRecord{}, err
	}
	rec := serviceRecord{
		ID:                  mustID(),
		EnvironmentID:       environment.ID,
		ProjectID:           environment.ProjectID,
		Name:                strings.TrimSpace(name),
		Spec:                spec,
		SpecRevision:        1,
		AllocatedAgentID:    agentID,
		DesiredReplicaCount: specReplicaCount(spec, defaultDesiredReplicaCount),
		CreatedAt:           now,
		UpdatedAt:           now,
		PendingChanges:      true,
	}
	if source := desiredSourceSpec(spec); source != nil {
		rec.SourceSummary = toProtoSourceStateSummary(source, nil, nil, nil)
	} else {
		rec.SourceSummary = buildSourceSummary(spec)
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(
			id, environment_id, name, current_spec_revision, current_rollout_generation,
			current_resolved_image, last_successful_commit_sha, latest_build_id,
			desired_replica_count, placement_message, created_at, updated_at
		) VALUES ($1, $2, $3, 1, 0, '', '', '', $4, '', $5, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.DesiredReplicaCount, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedService(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec, trustedAgentID string) (serviceRecord, []string, error) {
	var rec serviceRecord
	var affectedAgentIDs []string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		trustedAgentID = strings.TrimSpace(trustedAgentID)
		if trustedAgentID == "" {
			return errors.New("trusted agent id required for managed service")
		}
		var agentExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agents WHERE id = $1)`, trustedAgentID).Scan(&agentExists); err != nil {
			return err
		}
		if !agentExists {
			return fmt.Errorf("%w: trusted agent %s is not enrolled", errNoPlacementAvailable, trustedAgentID)
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
			return fmt.Errorf("%w: trusted agent %s is not dedicated to the managed dashboard", errNoPlacementAvailable, trustedAgentID)
		}
		current, found, err := s.serviceByNameQuerier(ctx, tx, environment.ID, name)
		if err != nil {
			return err
		}
		if !found {
			rec, err = s.createServiceTxInternal(ctx, tx, projectID, name, spec, trustedAgentID)
			affectedAgentIDs = []string{trustedAgentID}
			return err
		}
		spec = canonicalServiceSpec(spec)
		placementChanged := current.AllocatedAgentID != trustedAgentID
		trustedAllocationExists := false
		if placementChanged {
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(
					SELECT 1 FROM allocations
					 WHERE service_id = $1 AND agent_id = $2
					   AND rollout_state IN ($3, $4)
				)`,
				current.ID, trustedAgentID, allocationRolloutStarting, allocationRolloutServing,
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
		var existing []allocationRecord
		if placementChanged {
			existing, err = s.listAllocationsByServiceIDQuerier(ctx, tx, current.ID, true)
			if err != nil {
				return err
			}
			rolloutService := current
			rolloutService.Spec = spec
			if _, err := s.prepareReplacementRolloutTx(ctx, tx, rolloutService, existing, now); err != nil {
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
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", now); err != nil {
			return err
		}
		if _, err := s.insertDeploymentTx(ctx, tx, current.ID, deploymentStateScheduling, deploymentActor{Kind: deploymentCauseSystem}, reasonManagedSync, "Managed service synchronized", nextSpecRevision, nextRolloutGeneration, "", directImageRef(spec), "", now); err != nil {
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
			if _, err := s.insertAllocationTx(ctx, tx, rec, trustedAgentID, now); err != nil {
				return err
			}
			if _, err := s.advanceRolloutTx(ctx, tx, current.ID, now); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET desired_spec_revision = $1,
				        desired_rollout_generation = $2,
				        updated_at = $3
				  WHERE service_id = $4 AND agent_id = $5
				    AND rollout_state IN ($6, $7)`,
				nextSpecRevision, nextRolloutGeneration, now, current.ID, trustedAgentID,
				allocationRolloutStarting, allocationRolloutServing,
			); err != nil {
				return err
			}
		}
		if desiredSourceSpec(spec) != nil {
			if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
				return err
			}
		}
		if err := s.bumpAllDesiredRevisionsTx(ctx, tx); err != nil {
			return err
		}
		affectedAgentIDs = appendAllocationAgentIDs([]string{trustedAgentID}, existing...)
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, nil, err
	}
	if len(affectedAgentIDs) == 0 {
		return rec, nil, nil
	}
	allAgentIDs, err := s.agentIDs(ctx)
	if err != nil {
		return serviceRecord{}, nil, err
	}
	return rec, allAgentIDs, nil
}

func (s *Store) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, error) {
	if err := validatePort(targetPort); err != nil {
		return domainBindingRecord{}, err
	}
	var binding domainBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var agentID, environmentID string
		if err := tx.QueryRowContext(ctx, `SELECT a.agent_id, s.environment_id FROM allocations a JOIN services s ON s.id = a.service_id
			JOIN environments e ON e.id = s.environment_id WHERE s.id = $1 AND e.project_id = $2`, serviceID, projectID).Scan(&agentID, &environmentID); err != nil {
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
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, service_id, target_port, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, serviceID, targetPort, now, now,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{agentID}); err != nil {
				return err
			}
			binding = domainBindingRecord{
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
			return errDomainAlreadyExists
		case binding.ServiceID == serviceID && binding.TargetPort == targetPort:
			return nil
		default:
			agentIDs := []string{agentID}
			if binding.ServiceID != serviceID {
				previousIDs, err := s.agentIDsForServiceQuerier(ctx, tx, binding.ServiceID)
				if err != nil {
					return err
				}
				agentIDs = append(agentIDs, previousIDs...)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        updated_at = $3
				  WHERE hostname = $4`,
				serviceID, targetPort, now, hostname,
			); err != nil {
				return err
			}
			if err := s.bumpDesiredRevisionsTx(ctx, tx, agentIDs); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.TargetPort = targetPort
			binding.UpdatedAt = now
			return nil
		}
	})
	if err != nil {
		return domainBindingRecord{}, err
	}
	return binding, nil
}
