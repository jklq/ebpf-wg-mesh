//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func testDelivery(store *Store) *Delivery {
	return NewDelivery(store, nil, nil, nil)
}

func createService(ctx context.Context, store *Store, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	return testDelivery(store).createService(ctx, userID, environmentID, name, spec, agentID)
}

func createScheduledService(ctx context.Context, store *Store, userID, environmentID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	return testDelivery(store).createScheduledService(ctx, userID, environmentID, name, spec)
}

func updateService(ctx context.Context, store *Store, userID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
	return testDelivery(store).updateService(ctx, userID, serviceID, name, spec)
}

func deleteService(ctx context.Context, store *Store, userID, serviceID string) error {
	return testDelivery(store).deleteService(ctx, userID, serviceID)
}

func scaleService(ctx context.Context, store *Store, userID, serviceID string, desired int32) (serviceRecord, []allocationRecord, error) {
	return testDelivery(store).scaleService(ctx, userID, serviceID, desired)
}

func discardServiceChanges(ctx context.Context, store *Store, userID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error) {
	return testDelivery(store).discardServiceChanges(ctx, userID, serviceID, changeIDs, discardAll)
}

func desiredStateForAgent(ctx context.Context, store *Store, agentID string) (*agentv1.DesiredNodeState, error) {
	return testDelivery(store).DesiredStateForAgent(ctx, agentID)
}

func claimNextBuild(ctx context.Context, store *Store, builderID, builderName string, staleAfter time.Duration) (buildRunRecord, error) {
	return testDelivery(store).claimNextBuild(ctx, builderID, builderName, staleAfter)
}

func chooseAgentForService(ctx context.Context, store *Store, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	return testDelivery(store).chooseAgentForService(ctx, environmentID, spec)
}

func registerAgent(ctx context.Context, store *Store, hello *agentv1.AgentHello) (bool, error) {
	return testDelivery(store).RegisterAgent(ctx, hello)
}

func claimNextSourceWorkItem(ctx context.Context, store *Store, processorID string, staleAfter time.Duration) (sourceWorkItemRecord, error) {
	return (&GitHubCoordinator{store: store}).ClaimNextWorkItem(ctx, processorID, staleAfter)
}

func completeBuildForTest(ctx context.Context, store *Store, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) error {
	_, err := NewDelivery(store, nil, nil, nil).completeBuild(ctx, builderID, buildID, state, commitSHA, imageDigest, failureReason)
	return err
}

func applyDeploymentActionForTest(ctx context.Context, store *Store, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (serviceRecord, deploymentActionRecord, error) {
	service, record, _, err := NewDelivery(store, nil, nil, nil).applyDeploymentAction(ctx, userID, serviceID, deploymentID, action, idempotencyKey, allocationID)
	return service, record, err
}

func enqueueBuildForTest(ctx context.Context, store *Store, userID, serviceID, commitSHA string) (buildRunRecord, error) {
	var build buildRunRecord
	err := store.withTx(ctx, func(tx *sql.Tx) error {
		service, err := store.serviceByIDQuerier(ctx, tx, userID, serviceID)
		if err != nil {
			return err
		}
		build, err = testDelivery(store).enqueueBuildTx(
			ctx,
			tx,
			service,
			commitSHA,
			deploymentActor{Kind: deploymentCauseUser, ID: userID},
		)
		return err
	})
	return build, err
}

func releaseEnvironmentServiceForTest(ctx context.Context, store *Store, userID, environmentID, serviceID string) (serviceRecord, error) {
	services, _, err := releaseEnvironmentForTest(ctx, store, userID, environmentID)
	if err != nil {
		return serviceRecord{}, err
	}
	for _, service := range services {
		if service.ID == serviceID {
			return service, nil
		}
	}
	return serviceRecord{}, sql.ErrNoRows
}

// Release fixtures use the same authorized operation as the transport.
func releaseEnvironmentForTest(ctx context.Context, store *Store, userID, environmentID string) ([]serviceRecord, []string, error) {
	notifier := &releaseTestNotifier{}
	released, err := NewDelivery(store, notifier, nil, nil).ReleaseEnvironment(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), environmentID)
	services := make([]serviceRecord, 0, len(released))
	for _, result := range released {
		services = append(services, result.Service)
	}
	return services, notifier.agentIDs, err
}

type releaseTestNotifier struct{ agentIDs []string }

func (n *releaseTestNotifier) Notify(id string) { n.agentIDs = append(n.agentIDs, id) }
