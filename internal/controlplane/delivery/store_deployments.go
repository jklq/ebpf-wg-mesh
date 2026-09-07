package delivery

import (
	"context"
	"strconv"
)

func (s *persistence) listServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]DeploymentRecord, error) {
	service, err := s.serviceByIDQuerier(ctx, s.db, userID, serviceID)
	if err != nil {
		return nil, err
	}

	queryLimit := int(limit)
	if queryLimit <= 0 {
		queryLimit = 50
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+DeploymentSelectColumns+`
		   FROM deployments
		  WHERE service_id = $1
		  ORDER BY created_at DESC, rollout_generation DESC, id DESC
		  LIMIT $2`,
		service.ID, queryLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []DeploymentRecord
	for rows.Next() {
		rec, err := ScanDeploymentRow(rows)
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

func deploymentRecordIDForRollout(serviceID string, rolloutGeneration int64) string {
	return "rollout:" + serviceID + ":" + strconv.FormatInt(rolloutGeneration, 10)
}
