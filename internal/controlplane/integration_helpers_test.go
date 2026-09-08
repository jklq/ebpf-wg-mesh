//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func testDelivery(store *persistence) *testDeliveryHarness {
	return newTestDelivery(store, nil, nil, nil)
}

func createService(ctx context.Context, store *persistence, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).CreateService(ctx, userID, environmentID, name, spec, agentID)
}

func createScheduledService(ctx context.Context, store *persistence, userID, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).CreateScheduledService(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), environmentID, name, spec)
}

func updateService(ctx context.Context, store *persistence, userID, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
	return testDelivery(store).UpdateService(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), serviceID, name, spec)
}

func deleteService(ctx context.Context, store *persistence, userID, serviceID string) error {
	return testDelivery(store).DeleteService(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), serviceID)
}

func scaleService(ctx context.Context, store *persistence, userID, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
	service, allocs, _, err := testDelivery(store).ScaleService(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), serviceID, desired)
	return service, allocs, err
}

func discardServiceChanges(ctx context.Context, store *persistence, userID, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).DiscardServiceChanges(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), serviceID, changeIDs, discardAll)
}

func desiredStateForAgent(ctx context.Context, store *persistence, agentID string) (*agentv1.DesiredNodeState, error) {
	return testDelivery(store).DesiredStateForAgent(ctx, agentID)
}

func claimNextBuild(ctx context.Context, store *persistence, builderID, builderName string, staleAfter time.Duration) (deliverycore.BuildRunRecord, error) {
	return testDelivery(store).ClaimNextBuild(ctx, builderID, builderName, staleAfter)
}

func chooseAgentForService(ctx context.Context, store *persistence, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	return testDelivery(store).chooseAgentForService(ctx, environmentID, spec)
}

func registerAgent(ctx context.Context, store *persistence, hello *agentv1.AgentHello) (bool, error) {
	return testDelivery(store).RegisterAgent(ctx, hello)
}

func claimNextSourceWorkItem(ctx context.Context, store *persistence, processorID string, staleAfter time.Duration) (deliverycore.SourceWorkItemRecord, error) {
	return (&GitHubCoordinator{store: store.source}).ClaimNextWorkItem(ctx, processorID, staleAfter)
}

func completeBuildForTest(ctx context.Context, store *persistence, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) error {
	_, err := newTestDelivery(store, nil, nil, nil).CompleteBuild(ctx, builderID, buildID, state, commitSHA, imageDigest, failureReason)
	return err
}

func applyDeploymentActionForTest(ctx context.Context, store *persistence, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.ServiceRecord, deliverycore.DeploymentActionRecord, error) {
	result, err := testDelivery(store).ApplyDeploymentAction(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), serviceID, deploymentID, action, idempotencyKey, allocationID)
	if err != nil {
		return deliverycore.ServiceRecord{}, deliverycore.DeploymentActionRecord{}, err
	}
	var record deliverycore.DeploymentActionRecord
	err = store.db.QueryRowContext(ctx, `SELECT id,service_id,target_deployment_id,result_deployment_id,action,allocation_id,idempotency_key,requested_by_user_id,created_at FROM deployment_actions WHERE service_id=$1 AND idempotency_key=$2`, serviceID, idempotencyKey).Scan(&record.ID, &record.ServiceID, &record.TargetDeploymentID, &record.ResultDeploymentID, &record.Action, &record.AllocationID, &record.IdempotencyKey, &record.RequestedByUserID, &record.CreatedAt)
	return result.Service, record, err
}

func enqueueBuildForTest(ctx context.Context, store *persistence, userID, serviceID, commitSHA string) (deliverycore.BuildRunRecord, error) {
	binding, err := store.source.sourceBindingByServiceID(ctx, serviceID)
	if err != nil {
		return deliverycore.BuildRunRecord{}, err
	}
	result, err := testDelivery(store).QueueSourceBuild(ctx, binding, commitSHA, deliverycore.SourceSnapshotRecord{})
	return result.Build, err
}

func releaseEnvironmentServiceForTest(ctx context.Context, store *persistence, userID, environmentID, serviceID string) (deliverycore.ServiceRecord, error) {
	services, _, err := releaseEnvironmentForTest(ctx, store, userID, environmentID)
	if err != nil {
		return deliverycore.ServiceRecord{}, err
	}
	for _, service := range services {
		if service.ID == serviceID {
			return service, nil
		}
	}
	return deliverycore.ServiceRecord{}, sql.ErrNoRows
}

// Release fixtures use the same authorized operation as the transport.
func releaseEnvironmentForTest(ctx context.Context, store *persistence, userID, environmentID string) ([]deliverycore.ServiceRecord, []string, error) {
	notifier := &releaseTestNotifier{}
	released, err := newTestDelivery(store, notifier, nil, nil).ReleaseEnvironment(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), environmentID)
	services := make([]deliverycore.ServiceRecord, 0, len(released))
	for _, result := range released {
		services = append(services, result.Service)
	}
	return services, notifier.agentIDs, err
}

type releaseTestNotifier struct{ agentIDs []string }

func (n *releaseTestNotifier) Notify(id string) { n.agentIDs = append(n.agentIDs, id) }
