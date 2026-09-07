package delivery

import "context"

func (r *ReadModel) ListServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]DeploymentRecord, error) {
	return r.store.listServiceDeployments(ctx, userID, serviceID, limit)
}
