package delivery

import "context"

func (r *ReadModel) ListDomainBindings(ctx context.Context, userID, serviceID string) ([]DomainBindingRecord, error) {
	return r.store.listDomainBindings(ctx, userID, serviceID)
}
func (r *ReadModel) ServiceStatus(ctx context.Context, userID, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	return r.store.serviceStatus(ctx, userID, serviceID)
}
