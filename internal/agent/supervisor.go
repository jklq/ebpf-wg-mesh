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
)

type workloadSupervisor struct {
	agentID         string
	runtime         Runtime
	store           *localStateStore
	applyNodeConfig func(context.Context, *agentv1.AssignedNodeConfig) error

	reconcileMu sync.Mutex
	trigger     chan struct{}
	reports     chan struct{}
	ready       atomic.Bool
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

	if desired, err := s.store.desiredState(); err != nil {
		return err
	} else if desired != nil {
		s.reconcile(ctx, "startup-restore")
	}

	go s.run(ctx)
	if source, ok := s.runtime.(RuntimeEventSource); ok {
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
	diskTicker := time.NewTicker(diskEnforcementInterval)
	defer diskTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.trigger:
			s.reconcile(ctx, "desired-state")
		case <-ticker.C:
			s.reconcile(ctx, "safety-resync")
		case <-diskTicker.C:
			s.reconcile(ctx, "disk-enforcement")
		}
	}
}

func (s *workloadSupervisor) AcceptDesired(clusterID, sessionID string, state *agentv1.DesiredNodeState) (bool, error) {
	s.reconcileMu.Lock()
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

func (s *workloadSupervisor) ReconcileAcceptedDesired() { s.requestReconcile() }

func (s *workloadSupervisor) RestartManagedDashboard(ctx context.Context, state *agentv1.DesiredNodeState) error {
	restarter, ok := s.runtime.(managedDashboardRestartRuntime)
	if !ok {
		return errors.New("runtime cannot restart the managed dashboard after certificate renewal")
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	return restarter.RestartManagedDashboard(ctx, state)
}

func (s *workloadSupervisor) reconcile(ctx context.Context, source string) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
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
	allowCleanup := summary.Initialization == initializationReady
	report, err := s.runtime.ReconcileWithCleanup(ctx, desired, allowCleanup)
	if err != nil {
		slog.Error("runtime reconciliation failed", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "source", source, "error", err)
		return
	}
	if report == nil {
		slog.Error("runtime reconciliation returned no observation", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "source", source)
		return
	}
	report.AgentId = s.agentID
	report.RecoveryMode = !allowCleanup
	persisted, changed, err := s.store.recordReport(report)
	if err != nil {
		slog.Error("persist runtime observation", "agent_id", s.agentID, "cursor", desired.GetReconciliationCursor(), "error", err)
		return
	}
	if changed && persisted != nil {
		select {
		case s.reports <- struct{}{}:
		default:
		}
	}
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
