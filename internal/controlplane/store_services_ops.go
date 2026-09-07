package controlplane

import (
	"context"
	"database/sql"
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
