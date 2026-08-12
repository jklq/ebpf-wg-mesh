package controlplane

import (
	"context"
	"sort"
	"strconv"
	"time"
)

func (s *Store) listServiceDeployments(ctx context.Context, userID, projectID, serviceID string, limit int32) ([]deploymentRecord, error) {
	service, err := s.serviceByIDQuerier(ctx, s.db, userID, projectID, serviceID)
	if err != nil {
		return nil, err
	}

	queryLimit := int(limit)
	switch {
	case queryLimit <= 0:
		queryLimit = 50
	default:
		queryLimit *= 2
		if queryLimit < 10 {
			queryLimit = 10
		}
	}

	rollouts, err := s.listRolloutBackedDeployments(ctx, service.ID, queryLimit)
	if err != nil {
		return nil, err
	}
	builds, err := s.listBuildOnlyDeployments(ctx, service.ID, queryLimit)
	if err != nil {
		return nil, err
	}

	records := make([]deploymentRecord, 0, len(rollouts)+len(builds))
	records = append(records, rollouts...)
	records = append(records, builds...)
	for i := range records {
		records[i].IsCurrent = isCurrentDeploymentRecord(service, records[i])
	}

	sort.SliceStable(records, func(i, j int) bool {
		left := records[i]
		right := records[j]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		leftQueuedAt := deploymentQueuedAt(left)
		rightQueuedAt := deploymentQueuedAt(right)
		if !leftQueuedAt.Equal(rightQueuedAt) {
			return leftQueuedAt.After(rightQueuedAt)
		}
		if left.RolloutGeneration != right.RolloutGeneration {
			return left.RolloutGeneration > right.RolloutGeneration
		}
		return left.ID > right.ID
	})

	if limit > 0 && len(records) > int(limit) {
		records = records[:limit]
	}
	return records, nil
}

func (s *Store) listRolloutBackedDeployments(ctx context.Context, serviceID string, limit int) ([]deploymentRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT
			r.service_id,
			r.rollout_generation,
			r.spec_revision,
			r.reason,
			r.build_id,
			r.requested_by_user_id,
			r.created_at,
			COALESCE(b.id, ''),
			COALESCE(b.service_id, ''),
			COALESCE(b.commit_sha, ''),
			COALESCE(b.commit_message, ''),
			COALESCE(b.commit_author, ''),
			COALESCE(b.state, ''),
			COALESCE(b.builder_id, ''),
			COALESCE(b.image_digest, ''),
			COALESCE(b.failure_reason, ''),
			COALESCE(b.source_revision_id, ''),
			COALESCE(b.source_snapshot_id, ''),
			COALESCE(b.source_snapshot_digest, ''),
			COALESCE(b.target_rollout_generation, 0),
			COALESCE(b.build_recipe_json, '{}'::JSONB),
			COALESCE(b.queued_at, r.created_at),
			b.started_at,
			b.finished_at
		   FROM service_rollouts r
		   LEFT JOIN build_runs b ON b.id = r.build_id
		  WHERE r.service_id = $1
		  ORDER BY r.created_at DESC, r.rollout_generation DESC
		  LIMIT $2`,
		serviceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deploymentRecord
	for rows.Next() {
		var (
			rollout    serviceRolloutRecord
			build      buildRunRecord
			recipeJSON []byte
		)
		if err := rows.Scan(
			&rollout.ServiceID,
			&rollout.RolloutGeneration,
			&rollout.SpecRevision,
			&rollout.Reason,
			&rollout.BuildID,
			&rollout.RequestedByUserID,
			&rollout.CreatedAt,
			&build.ID,
			&build.ServiceID,
			&build.CommitSHA,
			&build.CommitMessage,
			&build.CommitAuthor,
			&build.State,
			&build.BuilderID,
			&build.ImageDigest,
			&build.FailureReason,
			&build.SourceRevisionID,
			&build.SourceSnapshotID,
			&build.SourceSnapshotDigest,
			&build.TargetRolloutGeneration,
			&recipeJSON,
			&build.QueuedAt,
			&build.StartedAt,
			&build.FinishedAt,
		); err != nil {
			return nil, err
		}
		var buildPtr *buildRunRecord
		if build.ID != "" {
			var err error
			build.BuildRecipe, err = unmarshalBuildRecipe(recipeJSON)
			if err != nil {
				return nil, err
			}
			buildCopy := build
			buildPtr = &buildCopy
		}
		out = append(out, deploymentRecord{
			ID:                deploymentRecordIDForRollout(rollout.ServiceID, rollout.RolloutGeneration),
			ServiceID:         rollout.ServiceID,
			RolloutGeneration: rollout.RolloutGeneration,
			SpecRevision:      rollout.SpecRevision,
			Reason:            rollout.Reason,
			CreatedAt:         rollout.CreatedAt,
			Build:             buildPtr,
			RequestedByUserID: rollout.RequestedByUserID,
		})
	}
	return out, rows.Err()
}

func (s *Store) listBuildOnlyDeployments(ctx context.Context, serviceID string, limit int) ([]deploymentRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+buildRunSelectColumns+`
		   FROM build_runs b
		  WHERE b.service_id = $1
		    AND b.state IN ($2, $3, $4, $5, $6)
		    AND NOT EXISTS (
		      SELECT 1
		        FROM service_rollouts r
		       WHERE r.build_id = b.id
		    )
		  ORDER BY b.queued_at DESC, b.id DESC
		  LIMIT $7`,
		serviceID,
		buildStateQueued,
		buildStateRunning,
		buildStateFailed,
		buildStateSuperseded,
		buildStateSucceeded,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []deploymentRecord
	for rows.Next() {
		build, err := scanBuildRunRow(rows)
		if err != nil {
			return nil, err
		}
		buildCopy := build
		out = append(out, deploymentRecord{
			ID:                "build:" + build.ID,
			ServiceID:         build.ServiceID,
			RolloutGeneration: build.TargetRolloutGeneration,
			Reason:            "build-" + build.State,
			CreatedAt:         build.QueuedAt,
			Build:             &buildCopy,
		})
	}
	return out, rows.Err()
}

func deploymentRecordIDForRollout(serviceID string, rolloutGeneration int64) string {
	return "rollout:" + serviceID + ":" + strconv.FormatInt(rolloutGeneration, 10)
}

func deploymentQueuedAt(rec deploymentRecord) time.Time {
	if rec.Build != nil {
		return rec.Build.QueuedAt
	}
	return rec.CreatedAt
}

func isCurrentDeploymentRecord(service serviceRecord, rec deploymentRecord) bool {
	if rec.Build != nil && rec.Build.ID != "" && rec.Build.ID == service.LatestBuildID {
		return true
	}
	return rec.RolloutGeneration > 0 && rec.RolloutGeneration == service.RolloutGeneration
}
