package delivery

import "context"

func (r *ReadModel) ProjectByIDInternalQuerier(ctx context.Context, q ServiceQueryer, projectID string) (ProjectRecord, error) {
	return r.store.projectByIDInternalQuerier(ctx, q, projectID)
}
