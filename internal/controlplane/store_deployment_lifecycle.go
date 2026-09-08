package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
)

func (s *readsPersistence) currentDeploymentForService(ctx context.Context, serviceID string) (deliverycore.DeploymentRecord, bool, error) {
	rec, err := deliverycore.ScanDeploymentRow(s.db.QueryRowContext(ctx,
		`SELECT `+deliverycore.DeploymentSelectColumns+`
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
