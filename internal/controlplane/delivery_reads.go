package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func (s *readsPersistence) serviceByID(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, error) {
	return s.deliveryQueries().ServiceByID(ctx, userID, serviceID)
}
func (s *readsPersistence) listServices(ctx context.Context, userID, environmentID string) ([]deliverycore.ServiceRecord, error) {
	return s.deliveryQueries().ListServices(ctx, userID, environmentID)
}
func (s *readsPersistence) listDomainBindings(ctx context.Context, userID, serviceID string) ([]deliverycore.DomainBindingRecord, error) {
	return s.deliveryQueries().ListDomainBindings(ctx, userID, serviceID)
}
func (s *readsPersistence) serviceStatus(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
	return s.deliveryQueries().ServiceStatus(ctx, userID, serviceID)
}
func (s *readsPersistence) listServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error) {
	return s.deliveryQueries().ListServiceDeployments(ctx, userID, serviceID, limit)
}
func (s *readsPersistence) listAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error) {
	return s.deliveryQueries().ListAllocationsByServiceID(ctx, serviceID)
}
func (s *readsPersistence) listAgents(ctx context.Context) ([]deliverycore.AgentRecord, error) {
	return s.deliveryQueries().ListAgents(ctx)
}
func (s *readsPersistence) environmentByID(ctx context.Context, userID, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.deliveryQueries().EnvironmentByID(ctx, userID, environmentID)
}
func (s *readsPersistence) duplicateEnvironment(ctx context.Context, userID, environmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error) {
	return s.delivery.DuplicateEnvironment(ctx, userID, environmentID, name, copyVariables)
}
