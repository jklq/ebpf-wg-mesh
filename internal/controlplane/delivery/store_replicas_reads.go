package delivery

import "context"

func (r *ReadModel) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceID(ctx, serviceID)
}
func (r *ReadModel) ListAllocationsByServiceIDQuerier(ctx context.Context, q ServiceQueryer, serviceID string, forUpdate bool) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceIDQuerier(ctx, q, serviceID, forUpdate)
}
