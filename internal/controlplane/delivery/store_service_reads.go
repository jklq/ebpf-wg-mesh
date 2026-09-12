package delivery

import (
	"context"
	"database/sql"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (s *persistence) listServices(ctx context.Context, userID, environmentID string) ([]ServiceRecord, error) {
	if _, err := s.environmentByID(ctx, userID, environmentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1
		  ORDER BY s.created_at ASC`,
		environmentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ServiceRecord
	for rows.Next() {
		rec, err := scanServiceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		spec, err := s.loadServiceDetails(ctx, out[i].ID, out[i].SpecRevision)
		if err != nil {
			return nil, err
		}
		out[i].Spec = spec
		out[i].SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, s.db, spec, out[i].ID)
		if err != nil {
			return nil, err
		}
		if out[i].ResolvedImage == "" && out[i].RolloutGeneration > 0 {
			out[i].ResolvedImage = directImageRef(spec)
		}
		out[i].LatestBuild, err = s.latestBuildForServiceQuerier(ctx, s.db, out[i].LatestBuildID)
		if err != nil {
			return nil, err
		}
		if err := s.attachLatestDeploymentQuerier(ctx, s.db, &out[i]); err != nil {
			return nil, err
		}
		out[i].UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, s.db, out[i].ID, out[i].Spec, out[i].RolloutGeneration)
		if err != nil {
			return nil, err
		}
		out[i].PendingChanges = len(out[i].UnappliedChanges) > 0
	}
	return out, nil
}

func (s *persistence) serviceByID(ctx context.Context, userID, serviceID string) (ServiceRecord, error) {
	return s.serviceByIDQuerier(ctx, s.db, userID, serviceID)
}

func (s *persistence) serviceByIDQuerier(ctx context.Context, q ServiceQueryer, userID, serviceID string) (ServiceRecord, error) {
	row := q.QueryRowContext(ctx,
		serviceSelectSQL+`
		   JOIN project_memberships m ON m.project_id = e.project_id
		  WHERE s.id = $1 AND m.user_id = $2 AND m.role IN ('owner', 'editor', 'viewer')`,
		serviceID, userID,
	)
	rec, err := scanServiceRow(row)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.SourceSummary, err = s.loadServiceSourceSummaryQuerier(ctx, q, rec.Spec, rec.ID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if rec.ResolvedImage == "" && rec.RolloutGeneration > 0 {
		rec.ResolvedImage = directImageRef(rec.Spec)
	}
	rec.LatestBuild, err = s.latestBuildForServiceQuerier(ctx, q, rec.LatestBuildID)
	if err != nil {
		return ServiceRecord{}, err
	}
	if err := s.attachLatestDeploymentQuerier(ctx, q, &rec); err != nil {
		return ServiceRecord{}, err
	}
	rec.UnappliedChanges, _, err = s.loadServiceUnappliedChangesQuerier(ctx, q, rec.ID, rec.Spec, rec.RolloutGeneration)
	if err != nil {
		return ServiceRecord{}, err
	}
	rec.PendingChanges = len(rec.UnappliedChanges) > 0
	return rec, nil
}

func (s *persistence) serviceByNameQuerier(ctx context.Context, q ServiceQueryer, environmentID, name string) (ServiceRecord, bool, error) {
	row := q.QueryRowContext(
		ctx,
		serviceSelectSQL+`
		  WHERE s.environment_id = $1 AND s.name = $2`,
		environmentID,
		name,
	)
	rec, err := scanServiceRow(row)
	switch {
	case err == nil:
		rec.Spec, err = s.loadServiceDetailsQuerier(ctx, q, rec.ID, rec.SpecRevision)
		if err != nil {
			return ServiceRecord{}, false, err
		}
		return rec, true, nil
	case err == sql.ErrNoRows:
		return ServiceRecord{}, false, nil
	default:
		return ServiceRecord{}, false, err
	}
}

const serviceSelectSQL = `SELECT s.id, s.environment_id, e.project_id, s.name, s.current_spec_revision,
		        COALESCE(ds.current_rollout_generation, 0),
		        COALESCE((SELECT a.agent_id FROM allocations a WHERE a.service_id = s.id AND a.rollout_state <> 'lost' ORDER BY CASE a.rollout_state WHEN 'serving' THEN 0 WHEN 'starting' THEN 1 ELSE 2 END, a.id LIMIT 1), ''),
		        COALESCE(ds.current_resolved_image, ''), COALESCE(ds.last_successful_commit_sha, ''), COALESCE(ds.latest_build_id, ''),
		        s.desired_replica_count, COALESCE(ds.placement_message, ''), s.created_at, GREATEST(s.updated_at, ds.updated_at)
		   FROM services s
		   JOIN service_delivery_status ds ON ds.service_id = s.id
		   JOIN environments e ON e.id = s.environment_id`

func scanServiceRow(scanner interface{ Scan(...any) error }) (ServiceRecord, error) {
	var rec ServiceRecord
	if err := scanner.Scan(
		&rec.ID,
		&rec.EnvironmentID,
		&rec.ProjectID,
		&rec.Name,
		&rec.SpecRevision,
		&rec.RolloutGeneration,
		&rec.AllocatedAgentID,
		&rec.ResolvedImage,
		&rec.LastSuccessfulCommitSHA,
		&rec.LatestBuildID,
		&rec.DesiredReplicaCount,
		&rec.PlacementMessage,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	); err != nil {
		return ServiceRecord{}, err
	}
	if rec.DesiredReplicaCount <= 0 {
		rec.DesiredReplicaCount = DefaultDesiredReplicaCount
	}
	rec.LatestBuild = nil
	return rec, nil
}

func (s *persistence) loadServiceDetails(ctx context.Context, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	return s.loadServiceDetailsQuerier(ctx, s.db, serviceID, specRevision)
}

func (s *persistence) loadServiceDetailsQuerier(ctx context.Context, q ServiceQueryer, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	var rawSpec []byte
	if err := q.QueryRowContext(ctx, `SELECT spec_json FROM service_revisions WHERE service_id = $1 AND spec_revision = $2`, serviceID, specRevision).Scan(&rawSpec); err != nil {
		return nil, err
	}
	return LoadServiceSpec(rawSpec)
}
