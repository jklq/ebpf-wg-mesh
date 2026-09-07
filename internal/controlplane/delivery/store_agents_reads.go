package delivery

import "context"

func (r *ReadModel) ListAgents(ctx context.Context) ([]AgentRecord, error) {
	return r.store.listAgents(ctx)
}
func (r *ReadModel) AgentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	return r.store.agentByID(ctx, agentID)
}
func (r *ReadModel) AgentIDs(ctx context.Context) ([]string, error) { return r.store.agentIDs(ctx) }
func (r *ReadModel) AgentIDsQuerier(ctx context.Context, q ServiceQueryer) ([]string, error) {
	return r.store.agentIDsQuerier(ctx, q)
}
func (r *ReadModel) SchedulerSnapshotTx(ctx context.Context, q ServiceQueryer) ([]AgentRecord, []ServiceRecord, error) {
	return r.store.schedulerSnapshotTx(ctx, q)
}
