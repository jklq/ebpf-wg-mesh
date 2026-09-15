//go:build integration

package controlplane

import (
	"context"
	"errors"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"github.com/google/uuid"
)

type testDeliveryHarness struct {
	*deliverycore.Delivery
	store       *persistence
	rolloutNow  func() time.Time
	failoverNow func() time.Time
}

func newTestDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *PlatformEvents) *testDeliveryHarness {
	return &testDeliveryHarness{Delivery: newDelivery(store, notifier, ingress, events, nil), store: store}
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
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		prepareTestStatusReport(ctx, d.store, id, report)
		ingress := &observedIngress{}
		err := newDelivery(d.store, nil, ingress, nil, nil).ObserveAgentStatus(ctx, id, report)
		if err == nil {
			return ingress.changed, nil, nil
		}
		if !errors.Is(err, deliverycore.ErrStaleObservation) {
			return false, nil, err
		}
		lastErr = err
	}
	return false, nil, lastErr
}

func prepareTestStatusReport(ctx context.Context, store *persistence, agentID string, report *agentv1.StatusReport) {
	if report == nil {
		return
	}
	report.AgentId = agentID
	if session, ok := fixtureLive(store).Session(agentID); ok {
		report.SessionId = session.SessionID
		report.ObservationSequence = session.Sequence + 1
	}
	for _, condition := range report.GetServices() {
		if condition.AllocationId == "" {
			continue
		}
		if condition.ServiceId == "" {
			_ = store.db.QueryRowContext(ctx, `SELECT service_id FROM allocation_assignments WHERE id = $1`, condition.AllocationId).Scan(&condition.ServiceId)
		}
		if condition.DesiredRolloutGeneration == 0 {
			_ = store.db.QueryRowContext(ctx, `SELECT desired_rollout_generation FROM allocation_assignments WHERE id = $1`, condition.AllocationId).Scan(&condition.DesiredRolloutGeneration)
		}
		if condition.DesiredSpecRevision == 0 {
			_ = store.db.QueryRowContext(ctx, `SELECT desired_spec_revision FROM allocation_assignments WHERE id = $1`, condition.AllocationId).Scan(&condition.DesiredSpecRevision)
		}
		if condition.AppliedSpecRevision == 0 {
			condition.AppliedSpecRevision = condition.DesiredSpecRevision
		}
		if condition.AllocationIpv4 == "" || condition.AllocationIpv6 == "" {
			var assignedIPv4, assignedIPv6 string
			_ = store.db.QueryRowContext(ctx, `SELECT allocation_ipv4, allocation_ipv6 FROM allocation_assignments WHERE id = $1`, condition.AllocationId).Scan(&assignedIPv4, &assignedIPv6)
			if condition.AllocationIpv4 == "" {
				condition.AllocationIpv4 = assignedIPv4
			}
			if condition.AllocationIpv6 == "" {
				condition.AllocationIpv6 = assignedIPv6
			}
		}
	}
}
func (d *testDeliveryHarness) UpdateFleetAgent(ctx context.Context, userID string, req *platformv1.UpdateAgentRequest) (deliverycore.AgentRecord, error) {
	return d.Delivery.UpdateFleetAgent(ctx, testUser(userID), req)
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
	service, err := createScheduledService(ctx, d.store, "user-1", environmentID, "placement-"+uuid.NewString(), spec)
	if err != nil {
		return "", err
	}
	_, err = releaseEnvironmentServiceForTest(ctx, d.store, "user-1", environmentID, service.ID)
	if err != nil {
		return "", err
	}
	allocations, err := d.store.reads.ListAllocationsByServiceID(ctx, service.ID)
	if err != nil {
		return "", err
	}
	if len(allocations) == 0 {
		return "", deliverycore.ErrNoPlacementAvailable
	}
	return allocations[0].AgentID, nil
}
func reportActiveForTest(ctx context.Context, s *persistence, serviceID string) error {
	allocations, err := s.reads.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, a := range allocations {
		report := &agentv1.StatusReport{AgentId: a.AgentID, Services: []*agentv1.ServiceCondition{{AllocationId: a.ID, ServiceId: a.ServiceID, DesiredRolloutGeneration: a.DesiredRolloutGeneration, AppliedSpecRevision: a.DesiredSpecRevision, AppliedRolloutGeneration: a.DesiredRolloutGeneration, Phase: "Running", Healthy: true, AllocationIpv4: a.AllocationIPv4, AllocationIpv6: a.AllocationIPv6, HealthyIpv4Ports: []int32{8080}, HealthyIpv6Ports: []int32{8080}}}}
		if _, _, err := testDelivery(s).recordStatusReport(ctx, a.AgentID, report); err != nil {
			return err
		}
	}
	return nil
}

type liveFixture interface {
	Admitted(string) bool
	Session(string) (deliverycore.AgentSession, bool)
	Observation(string, int64) (deliverycore.AllocationObservation, bool)
	RecordObservation(deliverycore.AllocationObservation) (deliverycore.ObservationOutcome, error)
	AcceptReport(string, string, uint64, []string, bool) error
	DesiredRevision(string) (int64, bool)
	SetLastContactForTest(string, time.Time)
}

func fixtureLive(store *persistence) liveFixture {
	return store.fleet.sessions.(liveFixture)
}
