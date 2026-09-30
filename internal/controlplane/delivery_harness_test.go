//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/google/uuid"
)

type testDeliveryHarness struct {
	*deliverycore.Delivery
	store       *persistence
	rolloutNow  func() time.Time
	failoverNow func() time.Time
}

func newTestDelivery(store *persistence, notifier deliverycore.PlatformNotifier, ingress deliverycore.PlatformIngress, events *platformEvents) *testDeliveryHarness {
	delivery := newDelivery(store, notifier, ingress, events, nil)
	// Tests pin tags without touching a registry; suites that control
	// tag movement install their own StaticResolver.
	delivery.SetImageResolver(registry.StaticResolverForTest())
	return &testDeliveryHarness{Delivery: delivery, store: store}
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

func (i *observedIngress) RequestSync() { i.changed = true }

func (i *observedIngress) Sync(context.Context) error { return nil }

func (i *observedIngress) Converged(context.Context) (bool, error) {
	return true, nil
}

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

func sourceSummaryForTest(ctx context.Context, s *persistence, id string) (*platformv1.ServiceSourceSummary, error) {
	service, err := s.reads.ServiceSnapshot(ctx, id)
	return service.SourceSummary, err
}

func (s *persistence) primaryAllocationForTest(ctx context.Context, serviceID string) (deliverycore.AllocationRecord, error) {
	allocations, err := s.reads.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return deliverycore.AllocationRecord{}, err
	}
	return primaryAllocation(allocations), nil
}

func (s *persistence) markAllocationHealthyForTest(ctx context.Context, serviceID, allocationIP string, healthyPorts ...int32) error {
	addressColumn, _ := testAllocationFamilyColumns(allocationIP)
	if err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE allocation_assignments SET %s = $1, rollout_state = $4, updated_at = $2 WHERE service_id = $3`, addressColumn),
			allocationIP, time.Now().UTC(), serviceID, deliverycore.AllocationRolloutServing,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE service_rollouts
			    SET state = $1, failure_reason = '', completed_at = $2, progress_at = $2
			  WHERE service_id = $3
			    AND rollout_generation = (SELECT current_rollout_generation FROM service_delivery_status WHERE service_id = $3)`,
			"succeeded", time.Now().UTC(), serviceID,
		); err != nil {
			return err
		}
		if err := recordServiceAssignmentsAndRollout(ctx, tx, serviceID); err != nil {
			return err
		}
		return seedActiveDeploymentTx(ctx, tx, serviceID)
	}); err != nil {
		return err
	}
	allocs, err := s.reads.ListAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, alloc := range allocs {
		if err := observeAllocationHealthyForTest(ctx, s, alloc, allocationIP, healthyPorts...); err != nil {
			return err
		}
	}
	return nil
}

func (s *persistence) markAllocationIDHealthyForTest(ctx context.Context, allocationID, allocationIP string, healthyPorts ...int32) error {
	addressColumn, _ := testAllocationFamilyColumns(allocationIP)
	if err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		if strings.TrimSpace(allocationIP) == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE allocation_assignments SET %s = $1, updated_at = $2 WHERE id = $3`, addressColumn),
			allocationIP, now, allocationID,
		); err != nil {
			return err
		}
		journal.AssignmentRow(allocationID).Capture(ctx)
		return nil
	}); err != nil {
		return err
	}
	var rec deliverycore.AllocationRecord
	if err := s.db.QueryRowContext(ctx, `SELECT id, service_id, agent_id, desired_spec_revision, desired_rollout_generation, allocation_ipv4, allocation_ipv6
		FROM allocation_assignments WHERE id = $1`, allocationID).Scan(
		&rec.ID, &rec.ServiceID, &rec.AgentID, &rec.DesiredSpecRevision, &rec.DesiredRolloutGeneration, &rec.AllocationIPv4, &rec.AllocationIPv6); err != nil {
		return err
	}
	return observeAllocationHealthyForTest(ctx, s, rec, allocationIP, healthyPorts...)
}

func observeAllocationHealthyForTest(ctx context.Context, store *persistence, alloc deliverycore.AllocationRecord, reportedIP string, healthyPorts ...int32) error {
	session, ok := fixtureLive(store).Session(alloc.AgentID)
	if !ok {
		return fmt.Errorf("no live session for agent %s", alloc.AgentID)
	}
	ipv4Ports, ipv6Ports := []int32(nil), []int32(nil)
	if _, col := testAllocationFamilyColumns(reportedIP); col == "healthy_ipv4_ports" {
		ipv4Ports = healthyPorts
	} else {
		ipv6Ports = healthyPorts
	}
	inventory, err := agentAllocationIDsForTest(ctx, store, alloc.AgentID)
	if err != nil {
		return err
	}
	if err := fixtureLive(store).AcceptReport(alloc.AgentID, session.SessionID, session.Sequence+1, inventory, true); err != nil {
		return err
	}
	_, err = fixtureLive(store).RecordObservation(deliverycore.AllocationObservation{
		AllocationID: alloc.ID, RolloutGeneration: alloc.DesiredRolloutGeneration,
		AppliedSpecRevision: alloc.DesiredSpecRevision, AppliedGeneration: alloc.DesiredRolloutGeneration,
		Phase: "Healthy", Healthy: true, HealthyIPv4Ports: ipv4Ports, HealthyIPv6Ports: ipv6Ports,
		AgentID: alloc.AgentID, SessionID: session.SessionID, Sequence: session.Sequence + 1, ObservedAt: time.Now().UTC(),
	})
	return err
}

// markAllocationServingForTest drives allocations to serving without
// activating the deployment, simulating an agent that serves traffic before
// the control plane observes the rollout active.
func markAllocationServingForTest(t *testing.T, store *persistence, serviceID, allocationIP string) {
	t.Helper()
	addressColumn, _ := testAllocationFamilyColumns(allocationIP)
	if _, err := store.db.ExecContext(context.Background(),
		fmt.Sprintf(`UPDATE allocation_assignments SET %s = $1, rollout_state = $3, updated_at = $2 WHERE service_id = $4`, addressColumn),
		allocationIP, time.Now().UTC(), deliverycore.AllocationRolloutServing, serviceID,
	); err != nil {
		t.Fatal(err)
	}
}

func testAllocationFamilyColumns(allocationIP string) (addressColumn, portsColumn string) {
	if addr, err := netip.ParseAddr(strings.TrimSpace(allocationIP)); err == nil && addr.Is4() {
		return "allocation_ipv4", "healthy_ipv4_ports"
	}
	return "allocation_ipv6", "healthy_ipv6_ports"
}

// agentAllocationIDsForTest returns the allocation IDs the control plane
// currently assigns to an agent, mirroring what the agent would report as its
// inventory so admission is re-evaluated correctly in fixtures.
func agentAllocationIDsForTest(ctx context.Context, store *persistence, agentID string) ([]string, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> 'lost' ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func seedActiveDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_transitions(id,deployment_id,from_state,to_state,cause_kind,cause_id,reason_code,detail,spec_revision,artifact_id,rollout_generation,occurred_at)
 SELECT $1,id,state,'active','system','','DEPLOYMENT_ACTIVE','Marked healthy for test',spec_revision,artifact_id,rollout_generation,statement_timestamp() FROM deployments WHERE service_id=$2 AND is_current AND state<>'active'`, uuid.NewString(), serviceID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE deployments SET state='active',cause_kind='system',reason_code='DEPLOYMENT_ACTIVE',detail='Marked healthy for test',updated_at=statement_timestamp() WHERE service_id=$1 AND is_current RETURNING id`, serviceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		journal.DeploymentRow(id).Capture(ctx)
	}
	return rows.Err()
}

func recordServiceAssignmentsAndRollout(ctx context.Context, tx *sql.Tx, serviceID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id::STRING FROM allocation_assignments WHERE service_id = $1`, serviceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		journal.AssignmentRow(id).Capture(ctx)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT current_rollout_generation FROM service_delivery_status WHERE service_id = $1`, serviceID).Scan(&generation); err != nil {
		return err
	}
	journal.RolloutRow(serviceID, generation).Capture(ctx)
	return nil
}

func (s *persistence) currentDesiredRevisionForAgent(ctx context.Context, id string) (int64, error) {
	_ = ctx
	rev, ok := fixtureLive(s).DesiredRevision(id)
	if !ok {
		return 0, sql.ErrNoRows
	}
	return rev, nil
}

func (s *persistence) countServiceRevisionsForTest(ctx context.Context, serviceID string) (int, error) {
	var revisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revisions); err != nil {
		return 0, err
	}
	return revisions, nil
}

func (s *persistence) countServiceRolloutsForTest(ctx context.Context, serviceID string) (int, error) {
	var rollouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_rollouts WHERE service_id = $1`, serviceID).Scan(&rollouts); err != nil {
		return 0, err
	}
	return rollouts, nil
}
