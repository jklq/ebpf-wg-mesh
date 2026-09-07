package delivery

import "context"

func (r *ReadModel) BuildRunByIDQuerier(ctx context.Context, q ServiceQueryer, buildID string) (BuildRunRecord, error) {
	return r.store.buildRunByIDQuerier(ctx, q, buildID)
}
func (r *ReadModel) ServiceByIDInternalQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByIDInternalQuerier(ctx, q, serviceID)
}
