package delivery

import "context"

func (r *ReadModel) EnvironmentByID(ctx context.Context, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.environmentByID(ctx, userID, environmentID)
}
func (r *ReadModel) AuthorizeEnvironmentWriteQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.authorizeEnvironmentWriteQuerier(ctx, q, userID, environmentID)
}
func (r *Delivery) DuplicateEnvironment(ctx context.Context, userID, sourceEnvironmentID, name string, copyVariables bool) (EnvironmentRecord, error) {
	return r.store.duplicateEnvironment(ctx, userID, sourceEnvironmentID, name, copyVariables)
}

func (r *ReadModel) EnvironmentByIDQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.environmentByIDQuerier(ctx, q, userID, environmentID)
}
