//go:build integration

package controlplane

import (
	"context"
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"time"
)

type testDeliveryHarness struct {
	*deliverycore.Delivery
	store       *persistence
	rolloutNow  func() time.Time
	failoverNow func() time.Time
}

func newTestDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) *testDeliveryHarness {
	return &testDeliveryHarness{Delivery: newDelivery(store, notifier, ingress, events), store: store}
}
func (d *testDeliveryHarness) ReconcileRollouts(ctx context.Context) error {
	d.SetClocks(d.rolloutNow, d.failoverNow)
	return d.Delivery.ReconcileRollouts(ctx)
}
func (d *testDeliveryHarness) ReconcileFailover(ctx context.Context, threshold time.Duration) (deliverycore.ServiceFailoverResult, error) {
	d.SetClocks(d.rolloutNow, d.failoverNow)
	return d.Delivery.ReconcileFailover(ctx, threshold)
}
func (d *testDeliveryHarness) failoverUnhealthyServices(ctx context.Context, now time.Time, threshold time.Duration) (deliverycore.ServiceFailoverResult, error) {
	d.failoverNow = func() time.Time { return now }
	return d.ReconcileFailover(ctx, threshold)
}
func (d *testDeliveryHarness) failoverServicesFromAgent(ctx context.Context, id string, cutoff time.Time) ([]string, []string, error) {
	d.failoverNow = func() time.Time { return cutoff.Add(deliverycore.AgentHealthyTTL) }
	result, err := d.ReconcileFailover(ctx, deliverycore.AgentHealthyTTL)
	return result.NotifyAgentIDs, result.EnvironmentIDs, err
}
func (d *testDeliveryHarness) listDesiredServices(ctx context.Context, id string) ([]*agentv1.DesiredService, error) {
	state, err := d.DesiredStateForAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	return state.Services, nil
}
func (d *testDeliveryHarness) assignedNodeConfigForAgent(ctx context.Context, id string) (*agentv1.AssignedNodeConfig, error) {
	state, err := d.DesiredStateForAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	return state.NodeConfig, nil
}

type observedIngress struct{ changed bool }

func (i *observedIngress) RequestSync()               { i.changed = true }
func (i *observedIngress) Sync(context.Context) error { return nil }
func (d *testDeliveryHarness) recordStatusReport(ctx context.Context, id string, report *agentv1.StatusReport) (bool, []string, error) {
	ingress := &observedIngress{}
	err := newDelivery(d.store, nil, ingress, nil).ObserveAgentStatus(ctx, id, report)
	return ingress.changed, nil, err
}
func (d *testDeliveryHarness) UpdateFleetAgent(ctx context.Context, userID string, req *platformv1.UpdateAgentRequest) (deliverycore.AgentRecord, error) {
	return d.Delivery.UpdateFleetAgent(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), req)
}
func (d *testDeliveryHarness) reconcileDrainingAgent(ctx context.Context, id string) ([]string, error) {
	err := d.ReconcileRollouts(ctx)
	return nil, err
}
func replicaCountPtr(value int32) *int32 { return &value }
func deploymentStatePreActive(state string) bool {
	switch state {
	case "staged", "queued_build", "building", "scheduling", "image_pull", "starting", "readiness":
		return true
	}
	return false
}
func deploymentStateTerminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "crashed", "removed", "superseded":
		return true
	}
	return false
}

func (d *testDeliveryHarness) chooseAgentForService(ctx context.Context, environmentID string, spec *platformv1.ServiceSpec) (string, error) {
	var projectEnvironment string
	if err := d.store.db.QueryRowContext(ctx, `SELECT id FROM environments WHERE project_id=$1 AND is_production`, environmentID).Scan(&projectEnvironment); err == nil {
		environmentID = projectEnvironment
	}
	service, err := createScheduledService(ctx, d.store, "user-1", environmentID, "placement-"+deliverycore.MustID(), spec)
	if err != nil {
		return "", err
	}
	_, err = releaseEnvironmentServiceForTest(ctx, d.store, "user-1", environmentID, service.ID)
	if err != nil {
		return "", err
	}
	allocations, err := d.store.reads.listAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		return "", err
	}
	if len(allocations) == 0 {
		return "", deliverycore.ErrNoPlacementAvailable
	}
	return allocations[0].AgentID, nil
}
func reportActiveForTest(ctx context.Context, s *persistence, serviceID string) error {
	allocations, err := s.reads.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, a := range allocations {
		err := testDelivery(s).ObserveAgentStatus(ctx, a.AgentID, &agentv1.StatusReport{AgentId: a.AgentID, Services: []*agentv1.ServiceCondition{{AllocationId: a.ID, AppliedSpecRevision: a.DesiredSpecRevision, AppliedRolloutGeneration: a.DesiredRolloutGeneration, Phase: "Running", Healthy: true, AllocationIpv4: a.AllocationIPv4, AllocationIpv6: a.AllocationIPv6, HealthyIpv4Ports: []int32{8080}, HealthyIpv6Ports: []int32{8080}}}})
		if err != nil {
			return err
		}
	}
	return nil
}
