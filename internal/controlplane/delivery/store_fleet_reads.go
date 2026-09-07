package delivery

import "context"

func (r *ReadModel) AuthorizeOperator(ctx context.Context, userID string) error {
	return r.store.authorizeOperator(ctx, userID)
}
