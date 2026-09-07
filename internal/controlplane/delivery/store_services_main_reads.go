package delivery

import (
	"context"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (r *ReadModel) ListServices(ctx context.Context, userID, environmentID string) ([]ServiceRecord, error) {
	return r.store.listServices(ctx, userID, environmentID)
}
func (r *ReadModel) ServiceByID(ctx context.Context, userID, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByID(ctx, userID, serviceID)
}
func (r *ReadModel) ServiceByIDQuerier(ctx context.Context, q ServiceQueryer, userID, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByIDQuerier(ctx, q, userID, serviceID)
}
func (r *ReadModel) LoadServiceDetailsQuerier(ctx context.Context, q ServiceQueryer, serviceID string, specRevision int64) (*platformv1.ServiceSpec, error) {
	return r.store.loadServiceDetailsQuerier(ctx, q, serviceID, specRevision)
}
