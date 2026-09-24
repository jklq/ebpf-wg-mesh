package controlplane

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/logs"
)

// resolveLogRetention maps service IDs to their project's log
// retention policy for the log pipeline's per-row expiry. Unknown
// services resolve to the zero policy, which keeps the platform
// default.
func (s *catalogPersistence) resolveLogRetention(ctx context.Context, serviceIDs []string) (map[string]logs.ProjectRetention, error) {
	out := make(map[string]logs.ProjectRetention, len(serviceIDs))
	if len(serviceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, e.project_id, COALESCE(p.log_retention_days, 0)
		   FROM services s
		   JOIN environments e ON e.id = s.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE s.id = ANY($1)`,
		serviceIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var serviceID string
		var policy logs.ProjectRetention
		if err := rows.Scan(&serviceID, &policy.ProjectID, &policy.RetentionDays); err != nil {
			return nil, err
		}
		out[serviceID] = policy
	}
	return out, rows.Err()
}
