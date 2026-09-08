package delivery

import (
	"context"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

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

func (r *ReadModel) BuildRunByIDQuerier(ctx context.Context, q ServiceQueryer, buildID string) (BuildRunRecord, error) {
	return r.store.buildRunByIDQuerier(ctx, q, buildID)
}
func (r *ReadModel) ServiceByIDInternalQuerier(ctx context.Context, q ServiceQueryer, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByIDInternalQuerier(ctx, q, serviceID)
}

func (r *ReadModel) ListServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]DeploymentRecord, error) {
	return r.store.listServiceDeployments(ctx, userID, serviceID, limit)
}

func (r *ReadModel) ListDomainBindings(ctx context.Context, userID, serviceID string) ([]DomainBindingRecord, error) {
	return r.store.listDomainBindings(ctx, userID, serviceID)
}
func (r *ReadModel) ServiceStatus(ctx context.Context, userID, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	return r.store.serviceStatus(ctx, userID, serviceID)
}

func (r *ReadModel) EnvironmentByID(ctx context.Context, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.environmentByID(ctx, userID, environmentID)
}
func (r *ReadModel) AuthorizeEnvironmentWriteQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.authorizeEnvironmentWriteQuerier(ctx, q, userID, environmentID)
}

func (r *ReadModel) EnvironmentByIDQuerier(ctx context.Context, q ServiceQueryer, userID, environmentID string) (EnvironmentRecord, error) {
	return r.store.environmentByIDQuerier(ctx, q, userID, environmentID)
}

func (r *ReadModel) AuthorizeOperator(ctx context.Context, userID string) error {
	return r.store.authorizeOperator(ctx, userID)
}

func (r *ReadModel) ProjectByIDInternalQuerier(ctx context.Context, q ServiceQueryer, projectID string) (ProjectRecord, error) {
	return r.store.projectByIDInternalQuerier(ctx, q, projectID)
}

func (r *ReadModel) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceID(ctx, serviceID)
}
func (r *ReadModel) ListAllocationsByServiceIDQuerier(ctx context.Context, q ServiceQueryer, serviceID string, forUpdate bool) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceIDQuerier(ctx, q, serviceID, forUpdate)
}

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
