package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"google.golang.org/protobuf/encoding/protojson"
)

const deploymentSelectColumns = `id, service_id, spec_revision, rollout_generation, build_id, image_digest,
	state, cause_kind, cause_id, reason_code, detail, resolved_spec_json, variable_versions_json,
	is_current, requested_by_user_id, created_at, updated_at`

func (s *readsPersistence) currentDeploymentForService(ctx context.Context, serviceID string) (deliverycore.DeploymentRecord, bool, error) {
	rec, err := scanDeploymentRow(s.db.QueryRowContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE`,
		serviceID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return deliverycore.DeploymentRecord{}, false, nil
	}
	if err != nil {
		return deliverycore.DeploymentRecord{}, false, err
	}
	return rec, true, nil
}

func scanDeploymentRow(scanner interface{ Scan(...any) error }) (deliverycore.DeploymentRecord, error) {
	var rec deliverycore.DeploymentRecord
	var resolvedSpecJSON, variableVersionsJSON []byte
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.BuildID,
		&rec.ImageDigest,
		&rec.State,
		&rec.CauseKind,
		&rec.CauseID,
		&rec.ReasonCode,
		&rec.Detail,
		&resolvedSpecJSON,
		&variableVersionsJSON,
		&rec.IsCurrent,
		&rec.RequestedByUserID,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return deliverycore.DeploymentRecord{}, err
	}
	rec.ResolvedSpec = &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(resolvedSpecJSON, rec.ResolvedSpec); err != nil {
		return deliverycore.DeploymentRecord{}, fmt.Errorf("decode deployment resolved spec: %w", err)
	}
	if err := json.Unmarshal(variableVersionsJSON, &rec.VariableVersions); err != nil {
		return deliverycore.DeploymentRecord{}, fmt.Errorf("decode deployment variable versions: %w", err)
	}
	if rec.VariableVersions == nil {
		rec.VariableVersions = map[string]int64{}
	}
	rec.Reason = rec.ReasonCode
	return rec, nil
}
