package delivery

import (
	"context"
	"database/sql"
	"github.com/google/uuid"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/protobuf/encoding/protojson"
)

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
			id, environment_id, name, current_spec_revision, current_rollout_generation,
			current_resolved_image, last_successful_commit_sha, latest_build_id,
			desired_replica_count, placement_message, created_at, updated_at
		) VALUES ($1, $2, $3, 1, 0, '', '', '', $4, '', $5, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.DesiredReplicaCount, now,
	); err != nil {
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
	return rec, nil
}
