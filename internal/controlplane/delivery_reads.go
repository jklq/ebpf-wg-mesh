package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func (s *Store) serviceByID(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, error) {
	return s.deliveryQueries().ServiceByID(ctx, userID, serviceID)
}
func (s *Store) listServices(ctx context.Context, userID, environmentID string) ([]deliverycore.ServiceRecord, error) {
	return s.deliveryQueries().ListServices(ctx, userID, environmentID)
}
func (s *Store) listDomainBindings(ctx context.Context, userID, serviceID string) ([]deliverycore.DomainBindingRecord, error) {
	return s.deliveryQueries().ListDomainBindings(ctx, userID, serviceID)
}
func (s *Store) serviceStatus(ctx context.Context, userID, serviceID string) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
	return s.deliveryQueries().ServiceStatus(ctx, userID, serviceID)
}
func (s *Store) listServiceDeployments(ctx context.Context, userID, serviceID string, limit int32) ([]deliverycore.DeploymentRecord, error) {
	return s.deliveryQueries().ListServiceDeployments(ctx, userID, serviceID, limit)
}
func (s *Store) listAllocationsByServiceID(ctx context.Context, serviceID string) ([]deliverycore.AllocationRecord, error) {
	return s.deliveryQueries().ListAllocationsByServiceID(ctx, serviceID)
}
func (s *Store) listAgents(ctx context.Context) ([]deliverycore.AgentRecord, error) {
	return s.deliveryQueries().ListAgents(ctx)
}
func (s *Store) environmentByID(ctx context.Context, userID, environmentID string) (deliverycore.EnvironmentRecord, error) {
	return s.deliveryQueries().EnvironmentByID(ctx, userID, environmentID)
}
func (s *Store) duplicateEnvironment(ctx context.Context, userID, environmentID, name string, copyVariables bool) (deliverycore.EnvironmentRecord, error) {
	return newDelivery(s, nil, nil, nil).DuplicateEnvironment(ctx, userID, environmentID, name, copyVariables)
}
