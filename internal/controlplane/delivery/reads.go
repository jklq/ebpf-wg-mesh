package delivery

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/authz"
)

func (r *readModel) ListAgents(ctx context.Context, user authz.User) ([]AgentRecord, error) {
	if _, err := r.store.authz.AuthorizeOperator(ctx, user); err != nil {
		return nil, err
	}
	return r.store.listAgents(ctx)
}
func (r *readModel) AgentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	return r.store.agentByID(ctx, agentID)
}
func (r *readModel) AgentIDs(ctx context.Context) ([]string, error) { return r.store.agentIDs(ctx) }

func (r *readModel) ListServiceDeployments(ctx context.Context, user authz.User, serviceID string, limit int32) ([]DeploymentRecord, error) {
	scope, err := r.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	return r.store.listServiceDeployments(ctx, scope, limit)
}

func (r *readModel) ListDomainBindings(ctx context.Context, user authz.User, serviceID string, includeDeleted bool) ([]DomainBindingRecord, error) {
	scope, err := r.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	return r.store.listDomainBindings(ctx, scope, includeDeleted)
}
func (r *readModel) ServiceStatus(ctx context.Context, user authz.User, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	scope, err := r.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return r.store.serviceStatus(ctx, scope)
}

func (r *readModel) EnvironmentByID(ctx context.Context, user authz.User, environmentID string) (EnvironmentRecord, error) {
	scope, err := r.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	return r.store.environmentByID(ctx, scope)
}

func (r *readModel) ListAllocationsByServiceID(ctx context.Context, serviceID string) ([]AllocationRecord, error) {
	return r.store.listAllocationsByServiceID(ctx, serviceID)
}

func (r *readModel) ListServices(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) ([]ServiceRecord, error) {
	scope, err := r.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return nil, err
	}
	return r.store.listServices(ctx, scope, includeDeleted)
}
func (r *readModel) ServiceByID(ctx context.Context, user authz.User, serviceID string) (ServiceRecord, error) {
	scope, err := r.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return ServiceRecord{}, err
	}
	return r.store.serviceByID(ctx, scope)
}

func (r *readModel) BuildByID(ctx context.Context, buildID string) (BuildRunRecord, error) {
	return r.store.buildRunByIDQuerier(ctx, r.store.db, buildID)
}
func (r *readModel) ServiceSnapshot(ctx context.Context, serviceID string) (ServiceRecord, error) {
	return r.store.serviceByIDInternalQuerier(ctx, r.store.db, serviceID)
}
