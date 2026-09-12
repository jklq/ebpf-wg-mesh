package delivery

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/authz"
)

func (s *persistence) listServiceDeployments(ctx context.Context, scope authz.Service, limit int32) ([]DeploymentRecord, error) {
	queryLimit := int(limit)
	if queryLimit <= 0 {
		queryLimit = 50
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1
		  ORDER BY created_at DESC, rollout_generation DESC, id DESC
		  LIMIT $2`,
		scope.ID(), queryLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []DeploymentRecord
	for rows.Next() {
		rec, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range records {
		transitions, err := s.loadDeploymentTransitions(ctx, s.db, records[i].ID)
		if err != nil {
			return nil, err
		}
		records[i].Transitions = transitions
		records[i].Actions, err = s.loadDeploymentActions(ctx, s.db, records[i].ID)
		if err != nil {
			return nil, err
		}
		if records[i].BuildID == "" {
			continue
		}
		build, err := s.buildRunByIDQuerier(ctx, s.db, records[i].BuildID)
		if err != nil {
			return nil, err
		}
		buildCopy := build
		records[i].Build = &buildCopy
	}
	return records, nil
}
