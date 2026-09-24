//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/source"
	"errors"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func storeTestArchive(ctx context.Context, store *persistence, archive []byte) (string, string, error) {
	digest, key, _, err := store.source.StoreSourceArchiveFromReader(ctx, bytes.NewReader(archive), int64(len(archive)))
	return digest, key, err
}

func testUser(id string) authz.User {
	user, err := authz.AuthenticatedUser(id)
	if err != nil {
		panic(err)
	}
	return user
}

func testDelivery(store *persistence) *testDeliveryHarness {
	return newTestDelivery(store, nil, nil, nil)
}

func bumpDesiredRevisionsForTest(t *testing.T, store *persistence, ctx context.Context, agentIDs []string) {
	t.Helper()
	if len(agentIDs) == 0 {
		return
	}
	if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return dbtx.BumpDesiredRevisions(ctx, tx, agentIDs)
	}); err != nil {
		t.Fatalf("bumpDesiredRevisions: %v", err)
	}
}

func createService(ctx context.Context, store *persistence, userID, environmentID, name string, spec *platformv1.ServiceSpec, agentID string) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).CreateService(ctx, testUser(userID), environmentID, name, spec, agentID)
}

func createScheduledService(ctx context.Context, store *persistence, userID, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).CreateScheduledService(ctx, testUser(userID), environmentID, name, spec)
}

func updateService(ctx context.Context, store *persistence, userID, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
	return testDelivery(store).UpdateService(ctx, testUser(userID), serviceID, name, spec)
}

func deleteService(ctx context.Context, store *persistence, userID, serviceID string) error {
	return testDelivery(store).DeleteService(ctx, testUser(userID), serviceID)
}

func scaleService(ctx context.Context, store *persistence, userID, serviceID string, desired int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, error) {
	service, allocs, _, err := testDelivery(store).ScaleService(ctx, testUser(userID), serviceID, desired)
	return service, allocs, err
}

func discardServiceChanges(ctx context.Context, store *persistence, userID, serviceID string, changeIDs []string, discardAll bool) (deliverycore.ServiceRecord, error) {
	return testDelivery(store).DiscardServiceChanges(ctx, testUser(userID), serviceID, changeIDs, discardAll)
}

func desiredStateForAgent(ctx context.Context, store *persistence, agentID string) (*agentv1.DesiredNodeState, error) {
	return testDelivery(store).DesiredStateForAgent(ctx, agentID)
}

func claimNextBuild(ctx context.Context, store *persistence, builderID, builderName string) (deliverycore.BuildRunRecord, error) {
	return testDelivery(store).ClaimNextBuild(ctx, builderID, builderName)
}

func chooseAgentForService(ctx context.Context, store *persistence, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	return testDelivery(store).chooseAgentForService(ctx, environmentID, spec)
}

func registerAgent(ctx context.Context, store *persistence, hello *agentv1.AgentHello) (bool, error) {
	if hello != nil && hello.LocalStoreId == "" {
		hello.LocalStoreId = "test-store-" + hello.GetAgentId()
	}
	if hello != nil && hello.GetSessionId() == "" {
		hello.SessionId = "test-session-" + hello.GetAgentId()
	}
	if hello != nil {
		if strings.TrimSpace(hello.WireguardEndpoint) == "" {
			hello.WireguardEndpoint = "192.0.2.10:51820"
		}
		if strings.TrimSpace(hello.AdvertiseAddr) == "" {
			hello.AdvertiseAddr = "fd00:30::"
		}
		if err := store.db.QueryRowContext(ctx, `SELECT session_incarnation + 1 FROM agent_registrations WHERE id = $1`, hello.GetAgentId()).Scan(&hello.SessionIncarnation); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return testDelivery(store).RegisterAgent(ctx, hello)
}

func completeBuildForTest(ctx context.Context, store *persistence, builderID, buildID string, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) error {
	build, err := store.reads.BuildByID(ctx, buildID)
	if err != nil {
		return err
	}
	_, err = newTestDelivery(store, nil, nil, nil).CompleteBuild(ctx, builderID, buildID, build.OwnerEpoch, state, commitSHA, imageDigest, failureReason)
	return err
}

func completeBuildForTestWithEpoch(ctx context.Context, store *persistence, builderID, buildID string, epoch int64, state platformv1.BuildState, commitSHA, imageDigest, failureReason string) error {
	_, err := newTestDelivery(store, nil, nil, nil).CompleteBuild(ctx, builderID, buildID, epoch, state, commitSHA, imageDigest, failureReason)
	return err
}

func applyDeploymentActionForTest(ctx context.Context, store *persistence, userID, serviceID, deploymentID string, action platformv1.DeploymentAction, idempotencyKey, allocationID string) (deliverycore.ServiceRecord, deliverycore.DeploymentActionRecord, error) {
	result, err := testDelivery(store).ApplyDeploymentAction(ctx, testUser(userID), serviceID, deploymentID, action, idempotencyKey, allocationID)
	if err != nil {
		return deliverycore.ServiceRecord{}, deliverycore.DeploymentActionRecord{}, err
	}
	var record deliverycore.DeploymentActionRecord
	err = store.db.QueryRowContext(ctx, `SELECT id,service_id,target_deployment_id,COALESCE(result_deployment_id,''),action,allocation_id,idempotency_key,requested_by_user_id,created_at FROM deployment_actions WHERE service_id=$1 AND idempotency_key=$2`, serviceID, idempotencyKey).Scan(&record.ID, &record.ServiceID, &record.TargetDeploymentID, &record.ResultDeploymentID, &record.Action, &record.AllocationID, &record.IdempotencyKey, &record.RequestedByUserID, &record.CreatedAt)
	return result.Service, record, err
}

func enqueueBuildForTest(ctx context.Context, store *persistence, userID, serviceID, commitSHA string) (deliverycore.BuildRunRecord, error) {
	binding, err := store.source.SourceBindingByServiceID(ctx, serviceID)
	if err != nil {
		return deliverycore.BuildRunRecord{}, err
	}
	queued, err := testDelivery(store).QueueSourceBuild(ctx, binding, commitSHA, source.SourceSnapshotRecord{}, source.BuildTransition{})
	if err != nil {
		return deliverycore.BuildRunRecord{}, err
	}
	return store.reads.BuildByID(ctx, queued.BuildID)
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

func releaseEnvironmentForTest(ctx context.Context, store *persistence, userID, environmentID string) ([]deliverycore.ServiceRecord, []string, error) {
	notifier := &releaseTestNotifier{}
	released, err := newTestDelivery(store, notifier, nil, nil).ReleaseEnvironment(ctx, testUser(userID), environmentID)
	services := make([]deliverycore.ServiceRecord, 0, len(released))
	for _, result := range released {
		services = append(services, result.Service)
	}
	return services, notifier.agentIDs, err
}

type releaseTestNotifier struct{ agentIDs []string }

func (n *releaseTestNotifier) Notify(id string) { n.agentIDs = append(n.agentIDs, id) }
