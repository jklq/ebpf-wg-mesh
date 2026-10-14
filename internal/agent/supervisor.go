package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"google.golang.org/protobuf/proto"
)

type workloadSupervisor struct {
	recoveryPaused      bool
	recoveryCheckpoints bool
	agentID             string
	runtime             Runtime
	store               *localStateStore
	applyNodeConfig     func(context.Context, *agentv1.AssignedNodeConfig) error

	reconcileMu     sync.Mutex
	cancelMu        sync.Mutex
	cancelReconcile context.CancelFunc
	updatesWaiting  int
	trigger         chan struct{}
	reports         chan struct{}
	ready           atomic.Bool
}

func newWorkloadSupervisor(agentID string, runtime Runtime, store *localStateStore, applyNodeConfig func(context.Context, *agentv1.AssignedNodeConfig) error) *workloadSupervisor {
	return &workloadSupervisor{
		agentID: agentID, runtime: runtime, store: store, applyNodeConfig: applyNodeConfig,
		trigger: make(chan struct{}, 1), reports: make(chan struct{}, 1),
	}
}

func (s *workloadSupervisor) Start(ctx context.Context, clusterID string) error {
	inventory, err := s.runtime.DiscoverRuntimeResources(ctx)
	if err != nil {
		return fmt.Errorf("discover runtime resources: %w", err)
	}
	if err := s.store.prepareStartup(clusterID, inventory); err != nil {
		return fmt.Errorf("prepare local state: %w", err)
	}
	summary, err := s.store.summary()
	if err != nil {
		return fmt.Errorf("read local state summary: %w", err)
	}
	if summary.QuarantinedStore != "" {
		slog.Error("local state was corrupt; destructive cleanup is fenced", "agent_id", s.agentID, "quarantined_path", summary.QuarantinedStore)
	}
	if summary.Initialization == initializationRecovery {
		slog.Warn("agent entered local-state recovery; unowned runtime resources will not be removed", "agent_id", s.agentID, "resources", len(inventory))
	} else {
		s.ready.Store(true)
	}

	desired, err := s.store.desiredState()
	if err != nil {
		return err
	}
	go func() {
		if desired != nil {
			s.reconcile(ctx, "startup-restore")
		}
		s.run(ctx)
	}()
	if disk, ok := s.runtime.(diskAccountingRuntime); ok && !s.recoveryPaused {
		go func() {
			ticker := time.NewTicker(diskEnforcementInterval)
			defer ticker.Stop()
			for {
				disk.RefreshDiskUsage(ctx)
				s.requestReconcile()
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if source, ok := s.runtime.(RuntimeEventSource); ok && !s.recoveryPaused {
		events, eventErrors := source.ReconcileEvents(ctx)
		go runtimeEventReconcileLoop(ctx, events, eventErrors, func() {
			s.requestReconcile()
		}, func(err error) {
			slog.Warn("runtime event watch ended", "agent_id", s.agentID, "error", err)
		})
	}
	return nil
}

func (s *workloadSupervisor) run(ctx context.Context) {
	ticker := time.NewTicker(reconcileSafetyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			s.reconcile(ctx, "desired-state")
		case <-ticker.C:
			s.reconcile(ctx, "safety-resync")
		}
	}
}

func (s *workloadSupervisor) AcceptDesired(clusterID, sessionID string, state *agentv1.DesiredNodeState) (bool, error) {
	if s.recoveryPaused && !s.recoveryCheckpoints {
		return false, errors.New("host administration has not authorized recovery checkpoints")
	}
	s.lockUpdate()
	defer s.reconcileMu.Unlock()
	changed, err := s.store.acceptDesired(clusterID, sessionID, state)
	if err != nil {
		return false, err
	}
	summary, err := s.store.summary()
	if err != nil {
		return false, err
	}
	s.ready.Store(summary.Initialization == initializationReady)
	return changed, nil
}

func (s *workloadSupervisor) AcceptDiff(clusterID, sessionID string, diff *agentv1.AllocationDiff) (bool, error) {
	if s.recoveryPaused {
		return false, errors.New("incremental updates remain paused during recovery")
	}
	s.lockUpdate()
	defer s.reconcileMu.Unlock()
	changed, err := s.store.acceptAllocationDiff(clusterID, sessionID, diff)
	if err != nil {
		return false, err
	}
	summary, err := s.store.summary()
	if err != nil {
		return false, err
	}
	s.ready.Store(summary.Initialization == initializationReady)
	return changed, nil
}

func (s *workloadSupervisor) AcceptNodeConfig(clusterID, sessionID string, update *agentv1.NodeConfigUpdate) (bool, error) {
	if s.recoveryPaused && !s.recoveryCheckpoints {
		return false, errors.New("host administration has not authorized recovery checkpoints")
	}
	s.lockUpdate()
	defer s.reconcileMu.Unlock()
	changed, err := s.store.acceptNodeConfigUpdate(clusterID, sessionID, update)
	if err != nil {
		return false, err
	}
	summary, err := s.store.summary()
	if err != nil {
		return false, err
	}
	s.ready.Store(summary.Initialization == initializationReady)
	return changed, nil
}

func (s *workloadSupervisor) AcceptCredentials(clusterID, sessionID string, creds *agentv1.PullCredentialSet) (bool, error) {
	if s.recoveryPaused && !s.recoveryCheckpoints {
		return false, errors.New("host administration has not authorized recovery checkpoints")
	}
	s.lockUpdate()
	defer s.reconcileMu.Unlock()
	return s.store.acceptPullCredentials(clusterID, sessionID, creds)
}

func (s *workloadSupervisor) AcceptReplicas(clusterID, sessionID string, replicas *agentv1.ReplicaEndpoints) (bool, error) {
	if s.recoveryPaused && !s.recoveryCheckpoints {
		return false, errors.New("host administration has not authorized recovery checkpoints")
	}
	// Discovery addresses do not change runtime desired state or report
	// generations. Persist them independently so churn cannot interrupt pulls.
	return s.store.acceptReplicaEndpoints(clusterID, sessionID, replicas)
}

func (s *workloadSupervisor) ReconcileAcceptedDesired() { s.requestReconcile() }

func (s *workloadSupervisor) RestartManagedDashboard(ctx context.Context, state *agentv1.DesiredNodeState) error {
	restarter, ok := s.runtime.(managedDashboardRestartRuntime)
	if !ok {
		return errors.New("runtime cannot restart the managed dashboard after certificate renewal")
	}
	s.lockUpdate()
	defer s.reconcileMu.Unlock()
	return restarter.RestartManagedDashboard(ctx, state)
}

// Updates interrupt slow runtime work before waiting for its lock. Tracking
// waiting updates also closes the race before a reconciliation registers its cancel.
func (s *workloadSupervisor) lockUpdate() {
	s.cancelMu.Lock()
	s.updatesWaiting++
	if s.cancelReconcile != nil {
		s.cancelReconcile()
	}
	s.cancelMu.Unlock()
	s.reconcileMu.Lock()
	s.cancelMu.Lock()
	s.updatesWaiting--
	s.cancelMu.Unlock()
}

func (s *workloadSupervisor) reconcile(ctx context.Context, source string) {
	if s.recoveryPaused && (!s.recoveryCheckpoints || source != "desired-state" && source != "startup-restore") {
		return
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.cancelMu.Lock()
	if s.updatesWaiting > 0 {
		s.cancelMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancelReconcile = cancel
	s.cancelMu.Unlock()
	defer func() {
		s.cancelMu.Lock()
		s.cancelReconcile = nil
		cancel()
		s.cancelMu.Unlock()
	}()
	desired, err := s.store.desiredState()
	if err != nil {
		slog.Error("load desired state for reconciliation", "agent_id", s.agentID, "source", source, "error", err)
		return
	}
	if desired == nil {
		return
	}
	if err := s.applyNodeConfig(ctx, desired.GetNodeConfig()); err != nil {
		slog.Error("apply persisted node configuration", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "source", source, "error", err)
		return
	}
	summary, err := s.store.summary()
	if err != nil {
		slog.Error("read local state before reconciliation", "agent_id", s.agentID, "error", err)
		return
	}
	allowCleanup := summary.Initialization == initializationReady && !s.recoveryPaused
	var report *agentv1.StatusReport
	if runtime, ok := s.runtime.(progressRuntime); ok {
		previous, readErr := s.store.currentReport()
		if readErr != nil {
			slog.Error("read prior runtime report", "error", readErr)
			return
		}
		if previous == nil {
			previous = &agentv1.StatusReport{Services: summary.Allocations}
		}
		report, err = runtime.ReconcileWithProgress(ctx, desired, allowCleanup, func(partial *agentv1.StatusReport) {
			if ctx.Err() != nil {
				return
			}
			previous = mergeProgressReport(desired, previous, partial)
			s.recordRuntimeReport(previous, summary, allowCleanup)
		})
	} else {
		report, err = s.runtime.ReconcileWithCleanup(ctx, desired, allowCleanup)
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Error("runtime reconciliation failed", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "source", source, "error", err)
		return
	}
	if report == nil {
		slog.Error("runtime reconciliation returned no observation", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "source", source)
		return
	}
	s.recordRuntimeReport(report, summary, allowCleanup)
	inventory, discoverErr := s.runtime.DiscoverRuntimeResources(ctx)
	if discoverErr != nil {
		slog.Warn("refresh runtime inventory", "agent_id", s.agentID, "error", discoverErr)
	} else if inventoryErr := s.store.recordRuntimeInventory(inventory); inventoryErr != nil {
		slog.Error("persist runtime inventory", "agent_id", s.agentID, "error", inventoryErr)
	}
}

func (s *workloadSupervisor) requestReconcile() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *workloadSupervisor) ReportNotifications() <-chan struct{} { return s.reports }

func (s *workloadSupervisor) CurrentReport() (*agentv1.StatusReport, error) {
	return s.store.currentReport()
}

func (s *workloadSupervisor) Summary() (localStateSummary, error) { return s.store.summary() }

func (s *workloadSupervisor) Ready() bool { return s.ready.Load() }

func (s *workloadSupervisor) recordRuntimeReport(report *agentv1.StatusReport, summary localStateSummary, allowCleanup bool) {
	stampReportGeneration(report, summary)
	report.AgentId = s.agentID
	report.RecoveryMode = !allowCleanup
	persisted, changed, err := s.store.recordReport(report)
	if err != nil {
		slog.Error("persist runtime observation", "agent_id", s.agentID, "cursor", report.GetReconciliationCursor(), "error", err)
		return
	}
	if changed && persisted != nil {
		select {
		case s.reports <- struct{}{}:
		default:
		}
	}
}

// Reports carry a complete desired inventory even while some workers are still
// running. Keep their last matching observation instead of withdrawing readiness.
func mergeProgressReport(desired *agentv1.DesiredNodeState, previous, partial *agentv1.StatusReport) *agentv1.StatusReport {
	conditions := make(map[string]*agentv1.ServiceCondition)
	for _, condition := range previous.GetServices() {
		conditions[condition.GetAllocationId()] = condition
	}
	for _, condition := range partial.GetServices() {
		conditions[condition.GetAllocationId()] = condition
	}
	report := &agentv1.StatusReport{AgentId: desired.GetAgentId(), Volumes: partial.GetVolumes()}
	for _, svc := range desired.GetServices() {
		condition := conditions[svc.GetAllocationId()]
		if condition == nil || condition.GetPhase() == "" || condition.GetDesiredSpecRevision() != svc.GetDesiredSpecRevision() || condition.GetDesiredRolloutGeneration() != svc.GetDesiredRolloutGeneration() {
			condition = &agentv1.ServiceCondition{AllocationId: svc.GetAllocationId(), ServiceId: svc.GetServiceId(), DesiredSpecRevision: svc.GetDesiredSpecRevision(), DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(), AllocationIpv4: svc.GetPrivateIpv4(), AllocationIpv6: svc.GetPrivateIpv6(), Phase: "Pending"}
		}
		report.Services = append(report.Services, proto.Clone(condition).(*agentv1.ServiceCondition))
	}
	return report
}
