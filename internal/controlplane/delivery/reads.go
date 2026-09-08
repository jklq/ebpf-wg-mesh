package delivery

import (
	"context"
)

func (r *readModel) ListAgents(ctx context.Context) ([]AgentRecord, error) {
	return r.store.listAgents(ctx)
}
func (r *readModel) AgentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	return r.store.agentByID(ctx, agentID)
}
func (r *readModel) AgentIDs(ctx context.Context) ([]string, error) { return r.store.agentIDs(ctx) }

func (r *readModel) ListServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]DeploymentRecord, error) {
	return r.store.listServiceDeployments(ctx, userID, serviceID, limit)
}

func (r *readModel) ListDomainBindings(ctx context.Context, userID, serviceID string) ([]DomainBindingRecord, error) {
	return r.store.listDomainBindings(ctx, userID, serviceID)
}
func (r *readModel) ServiceStatus(ctx context.Context, userID, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	return r.store.serviceStatus(ctx, userID, serviceID)
}

func (r *readModel) EnvironmentByID(ctx context.Context, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.environmentByID(ctx, userID, environmentID)
}

func (r *readModel) AuthorizeOperator(ctx context.Context, userID string) error {
	return r.store.authorizeOperator(ctx, userID)
}

func (r *readModel) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceID(ctx, serviceID)
}

func (r *readModel) ListServices(ctx context.Context, userID, environmentID string) ([]ServiceRecord, error) {
	return r.store.listServices(ctx, userID, environmentID)
}
func (r *readModel) ServiceByID(ctx context.Context, userID, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByID(ctx, userID, serviceID)
}

func (r *readModel) BuildByID(ctx context.Context, buildID string) (BuildRunRecord, error) {
	return r.store.buildRunByIDQuerier(ctx, r.store.db, buildID)
}
func (r *readModel) ServiceSnapshot(ctx context.Context, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByIDInternalQuerier(ctx, r.store.db, serviceID)
}
