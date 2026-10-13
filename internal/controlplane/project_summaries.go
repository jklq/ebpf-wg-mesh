package controlplane

import (
	"context"
	"fmt"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

// Overview reads fetch only names and identities; service specs and secrets
// are unnecessary here. Both queries retain the caller's membership checks.
func (s *catalogPersistence) listProjectSummaries(ctx context.Context, user authz.User, projects []deliverycore.ProjectRecord) ([]*platformv1.ProjectSummary, error) {
	out := make([]*platformv1.ProjectSummary, 0, len(projects))
	if len(projects) == 0 {
		return out, nil
	}
	byProject := make(map[string]*platformv1.ProjectSummary, len(projects))
	args := []any{user.ID()}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		summary := &platformv1.ProjectSummary{Project: toProtoProject(project)}
		out = append(out, summary)
		byProject[project.ID] = summary
		args = append(args, project.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.project_id, e.name
		FROM environments e JOIN projects p ON p.id=e.project_id
		JOIN workspace_memberships m ON m.workspace_id=p.workspace_id
		WHERE m.user_id=$1 AND p.id IN (`+strings.Join(ids, ",")+`)
		AND p.deleted_at IS NULL AND e.deleted_at IS NULL
		ORDER BY e.is_production DESC, e.created_at ASC, e.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var environment platformv1.ProjectEnvironmentSummary
		var projectID string
		if err := rows.Scan(&environment.Id, &projectID, &environment.Name); err != nil {
			rows.Close()
			return nil, err
		}
		summary := byProject[projectID]
		summary.EnvironmentCount++
		if summary.LandingEnvironment == nil {
			summary.LandingEnvironment = &environment
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	byEnvironment := make(map[string]*platformv1.ProjectSummary)
	args = []any{user.ID()}
	ids = ids[:0]
	for _, summary := range out {
		if summary.LandingEnvironment == nil {
			continue
		}
		id := summary.LandingEnvironment.Id
		byEnvironment[id] = summary
		args = append(args, id)
		ids = append(ids, fmt.Sprintf("$%d", len(args)))
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err = s.db.QueryContext(ctx, `SELECT s.id, s.name, s.environment_id
		FROM services s JOIN environments e ON e.id=s.environment_id
		JOIN projects p ON p.id=e.project_id
		JOIN workspace_memberships m ON m.workspace_id=p.workspace_id
		WHERE m.user_id=$1 AND e.id IN (`+strings.Join(ids, ",")+`)
		AND p.deleted_at IS NULL AND e.deleted_at IS NULL AND s.deleted_at IS NULL
		ORDER BY s.created_at ASC, s.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var service platformv1.ProjectServiceSummary
		var environmentID string
		if err := rows.Scan(&service.Id, &service.Name, &environmentID); err != nil {
			return nil, err
		}
		summary := byEnvironment[environmentID]
		summary.Services = append(summary.Services, &service)
	}
	return out, rows.Err()
}
