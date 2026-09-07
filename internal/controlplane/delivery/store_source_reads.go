package delivery

import "context"

func (r *ReadModel) SourceBindingByServiceIDQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (SourceBindingRecord, error) {
	return r.store.sourceBindingByServiceIDQuerier(ctx, q, serviceID)
}
func (r *ReadModel) SourceRevisionByBindingAndCommitTx(ctx context.Context, q ServiceQueryer, sourceBindingID, commitSHA string) (SourceRevisionRecord, error) {
	return r.store.sourceRevisionByBindingAndCommitTx(ctx, q, sourceBindingID, commitSHA)
}
func (r *ReadModel) SourceSnapshotByProviderRepoAndCommitTx(ctx context.Context, q ServiceQueryer, provider, repositoryExternalID, commitSHA string) (SourceSnapshotRecord, error) {
	return r.store.sourceSnapshotByProviderRepoAndCommitTx(ctx, q, provider, repositoryExternalID, commitSHA)
}
func (r *ReadModel) SourceSnapshotByRevisionIDTx(ctx context.Context, q ServiceQueryer, sourceRevisionID string) (SourceSnapshotRecord, error) {
	return r.store.sourceSnapshotByRevisionIDTx(ctx, q, sourceRevisionID)
}
