package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/recovery"
	"ebof-wg-mesh/internal/restartpolicy"
	"ebof-wg-mesh/internal/runtimeutil"
	"ebof-wg-mesh/internal/volumestore"
)

const defaultCPUCFSPeriod uint64 = 100_000

type serviceEngine interface {
	DiscoverServices(context.Context) ([]RuntimeResource, error)
	EnsureService(context.Context, *agentv1.DesiredService) (serviceStatus, bool, error)
	// InspectService reports the container's current status without creating one.
	// exists=false when no container matches the desired generation and network
	// identity, missing or stale.
	InspectService(context.Context, *agentv1.DesiredService) (serviceStatus, bool, error)
	DrainService(context.Context, string, time.Time) (bool, bool, error)
	RemoveService(context.Context, string) error
	SetLogSink(LogSink)
	Close() error
}

type serviceStatus struct {
	AppliedSpecRevision      int64
	AppliedRolloutGeneration int64
	AllocationIPv4           string
	AllocationIPv6           string
	NetworkNamespacePath     string
	Running                  bool
	ExitCode                 int32
	Signal                   int32
	OOMKilled                bool
	DiskExhausted            bool
}

type ContainerdRuntime struct {
	cfg         config.AgentConfig
	engine      serviceEngine
	volumes     *volumestore.Store
	probeHealth func(context.Context, string, string, *agentv1.DesiredService) serviceHealthProbe
	ready       map[string]rolloutReadiness
	clock       restartpolicy.Clock
	rng         *rand.Rand
	rngMu       sync.Mutex
	forceStart  map[string]bool
}

type rolloutReadiness struct {
	rolloutGeneration int64
	healthyIPv4Ports  []int32
	healthyIPv6Ports  []int32
}

func NewContainerdRuntime(cfg config.AgentConfig) (*ContainerdRuntime, error) {
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.DataDir, "desired"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir desired dir: %w", err)
	}
	volumes, err := volumestore.New(cfg.Runtime.VolumesDir, volumestore.Backend(cfg.Runtime.VolumeBackend))
	if err != nil {
		return nil, err
	}
	if secretsDir := cfg.Runtime.ManagedDashboardSecretsDir; secretsDir != "" {
		info, err := os.Stat(secretsDir)
		if err != nil {
			return nil, fmt.Errorf("stat managed dashboard secrets dir: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("managed dashboard secrets path is not a directory")
		}
	}
	engine, err := newContainerdEngine(cfg, volumes)
	if err != nil {
		return nil, err
	}
	return &ContainerdRuntime{
		cfg:     cfg,
		engine:  engine,
		volumes: volumes,
		clock:   restartpolicy.SystemClock{},
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// SetRecoveryPaused suppresses image cleanup even during approved checkpoint
// application. Normal supervision resumes by restarting with host admission.
func (r *ContainerdRuntime) SetRecoveryPaused(paused bool) {
	if engine, ok := r.engine.(interface{ SetRecoveryPaused(bool) }); ok {
		engine.SetRecoveryPaused(paused)
	}
}

func (r *ContainerdRuntime) now() time.Time {
	if r != nil && r.clock != nil {
		return r.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *ContainerdRuntime) randSource() *rand.Rand {
	if r != nil && r.rng != nil {
		return r.rng
	}
	return rand.New(rand.NewSource(r.now().UnixNano()))
}

func (r *ContainerdRuntime) Close() error {
	if r == nil || r.engine == nil {
		return nil
	}
	return r.engine.Close()
}

func (r *ContainerdRuntime) ReconcileEvents(ctx context.Context) (<-chan struct{}, <-chan error) {
	if r == nil || r.engine == nil {
		return nil, nil
	}
	if source, ok := r.engine.(RuntimeEventSource); ok {
		return source.ReconcileEvents(ctx)
	}
	return nil, nil
}

func (r *ContainerdRuntime) SetLogSink(sink LogSink) {
	if r == nil || r.engine == nil {
		return
	}
	r.engine.SetLogSink(sink)
}

func (r *ContainerdRuntime) RestartManagedDashboard(ctx context.Context, state *agentv1.DesiredNodeState) error {
	for _, svc := range state.GetServices() {
		if !isManagedDashboardService(svc) {
			continue
		}
		if r.forceStart == nil {
			r.forceStart = make(map[string]bool)
		}
		r.forceStart[svc.GetAllocationId()] = true
		if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
			return fmt.Errorf("restart managed dashboard: %w", err)
		}
	}
	return nil
}

func (r *ContainerdRuntime) Reconcile(ctx context.Context, state *agentv1.DesiredNodeState) (*agentv1.StatusReport, error) {
	return r.ReconcileWithCleanup(ctx, state, true)
}

func (r *ContainerdRuntime) DiscoverRuntimeResources(ctx context.Context) ([]RuntimeResource, error) {
	if r == nil || r.engine == nil {
		return nil, nil
	}
	return r.engine.DiscoverServices(ctx)
}

// volumeStore is built from configuration on first use when the runtime was
// assembled without NewContainerdRuntime.
func (r *ContainerdRuntime) volumeStore() (*volumestore.Store, error) {
	if r.volumes != nil {
		return r.volumes, nil
	}
	backend := volumestore.Backend(r.cfg.Runtime.VolumeBackend)
	if backend == "" {
		backend = volumestore.BackendDirectory
	}
	volumes, err := volumestore.New(r.cfg.Runtime.VolumesDir, backend)
	if err != nil {
		return nil, err
	}
	r.volumes = volumes
	return volumes, nil
}

func (r *ContainerdRuntime) ReconcileWithCleanup(ctx context.Context, state *agentv1.DesiredNodeState, allowCleanup bool) (*agentv1.StatusReport, error) {
	report := &agentv1.StatusReport{AgentId: state.GetAgentId()}
	desiredServices := runtimeutil.IndexDesiredServices(state.GetServices())

	// Stale allocations stop before volumes reconcile so a destroyed volume's
	// last workload has released its mount.
	if allowCleanup {
		if err := r.pruneStaleServices(ctx, desiredServices); err != nil {
			return nil, err
		}
	}
	volumes, err := r.volumeStore()
	if err != nil {
		return nil, err
	}
	report.Volumes = volumes.Reconcile(state.GetVolumes(), allowCleanup)

	pinned := make(map[string]bool, len(state.GetVolumes()))
	for _, volume := range state.GetVolumes() {
		if !volume.GetDestroy() {
			pinned[volume.GetVolumeId()] = true
		}
	}
	for _, svc := range state.GetServices() {
		cond := r.reconcileService(ctx, svc, pinned)
		report.Services = append(report.Services, cond)
	}
	return report, nil
}

func (r *ContainerdRuntime) reconcileService(ctx context.Context, svc *agentv1.DesiredService, pinnedVolumes map[string]bool) *agentv1.ServiceCondition {
	cond := &agentv1.ServiceCondition{
		AllocationId:             svc.GetAllocationId(),
		ServiceId:                svc.GetServiceId(),
		DesiredSpecRevision:      svc.GetDesiredSpecRevision(),
		DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		AllocationIpv4:           svc.GetPrivateIpv4(),
		AllocationIpv6:           svc.GetPrivateIpv6(),
		Phase:                    "Pending",
	}
	if err := validateRuntimeID("allocation ID", svc.GetAllocationId()); err != nil {
		cond.Phase = "Error"
		cond.Message = err.Error()
		return cond
	}
	if err := r.persistDesiredService(svc); err != nil {
		cond.Phase = "Error"
		cond.Message = err.Error()
		return cond
	}
	if svc.GetIntent() == agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN {
		deadline := svc.GetDrainDeadline()
		if deadline == nil || !deadline.IsValid() {
			cond.Phase = "Error"
			cond.Message = "drain deadline is required"
			return cond
		}
		drained, forced, err := r.engine.DrainService(ctx, svc.GetAllocationId(), deadline.AsTime())
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			return cond
		}
		cond.AppliedSpecRevision = svc.GetDesiredSpecRevision()
		cond.AppliedRolloutGeneration = svc.GetDesiredRolloutGeneration()
		cond.AllocationIpv4 = svc.GetPrivateIpv4()
		cond.AllocationIpv6 = svc.GetPrivateIpv6()
		cond.Healthy = false
		// Preserve crash evidence across the drain: a nil restart would drop the
		// prior exit cause and count from the draining overlay.
		cond.Restart = r.loadObservation(svc.GetAllocationId(), svc.GetRestartObservation())
		if drained {
			cond.Phase = "Drained"
			if forced {
				cond.Message = "drain deadline elapsed; workload was force killed"
			} else {
				cond.Message = "workload exited after SIGTERM"
			}
		} else {
			cond.Phase = "Draining"
			cond.Message = "SIGTERM sent; waiting for graceful shutdown"
		}
		return cond
	}
	if err := r.checkVolumeAvailable(svc, pinnedVolumes); err != nil {
		cond.Phase = "Error"
		cond.Message = err.Error()
		return cond
	}
	obs := r.loadObservation(svc.GetAllocationId(), svc.GetRestartObservation())
	operatorNonce := svc.GetOperatorRestartNonce()
	if r.forceStart[svc.GetAllocationId()] {
		if obs.GetAppliedOperatorRestartNonce() >= operatorNonce {
			operatorNonce = obs.GetAppliedOperatorRestartNonce() + 1
		}
		delete(r.forceStart, svc.GetAllocationId())
	}
	var status serviceStatus
	var created bool
	if shouldPreserveTerminalObservation(r.now(), svc.GetSpec().GetRuntime().GetRestart(), obs, svc.GetDesiredRolloutGeneration(), operatorNonce) {
		// A terminal allocation must never execute: resolve status without creating
		// a container, so a missing container stays missing on every resync.
		preserved, err := r.inspectPreservedService(ctx, svc, obs)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			return cond
		}
		status = preserved
	} else {
		var err error
		status, created, err = r.engine.EnsureService(ctx, svc)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			return cond
		}
		if created {
			status.Running = true
			delete(r.ready, svc.GetAllocationId())
		}
	}
	if status.DiskExhausted {
		// Exhaustion is measured from the snapshot, so it stays true after the
		// writer exits. Reclaim it either way: gating this on a live task lets an
		// allocation that already died hold its over-quota snapshot for the whole
		// backoff window, which is the host disk this check exists to protect.
		if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			return cond
		}
		delete(r.ready, svc.GetAllocationId())
		status.Running = false
	}
	livenessFailed := false
	if status.Running {
		if reason, failed := r.livenessFailed(ctx, status, svc); failed {
			livenessFailed = true
			if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
				cond.Phase = "Error"
				cond.Message = err.Error()
				return cond
			}
			delete(r.ready, svc.GetAllocationId())
			status.Running = false
			_ = reason
		}
	}
	r.rngMu.Lock()
	decision := restartpolicy.Evaluate(r.now(), r.randSource(), svc.GetSpec().GetRuntime().GetRestart(), obs, restartpolicy.Input{
		State: restartpolicy.ProcessState{
			Running:       status.Running,
			ExitCode:      status.ExitCode,
			Signal:        status.Signal,
			OOMKilled:     status.OOMKilled,
			DiskExhausted: status.DiskExhausted,
		},
		LivenessFailed:           livenessFailed,
		DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		OperatorRestartNonce:     operatorNonce,
	})
	r.rngMu.Unlock()
	if err := r.saveObservation(svc.GetAllocationId(), decision.Observation); err != nil {
		cond.Phase = "Error"
		cond.Message = err.Error()
		return cond
	}
	cond.Restart = decision.Observation
	switch decision.Action {
	case restartpolicy.ActionCrashLoop, restartpolicy.ActionStop, restartpolicy.ActionWait:
		cond.AppliedSpecRevision = status.AppliedSpecRevision
		cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
		cond.AllocationIpv4 = status.AllocationIPv4
		cond.AllocationIpv6 = status.AllocationIPv6
		cond.Healthy = false
		cond.Phase = decision.Phase
		cond.Message = decision.Message
		return cond
	case restartpolicy.ActionStart:
		if !status.Running || livenessFailed {
			if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
				cond.Phase = "Error"
				cond.Message = err.Error()
				return cond
			}
			delete(r.ready, svc.GetAllocationId())
			var err error
			status, created, err = r.engine.EnsureService(ctx, svc)
			if err != nil {
				cond.Phase = "Error"
				cond.Message = err.Error()
				return cond
			}
			if created {
				status.Running = true
			}
			if !status.Running && decision.Observation != nil {
				decision.Observation.AwaitingRestart = false
				if err := r.saveObservation(svc.GetAllocationId(), decision.Observation); err != nil {
					cond.Phase = "Error"
					cond.Message = err.Error()
					return cond
				}
			}
		}
	}
	return r.finishRunningCondition(ctx, cond, svc, status, created)
}

func (r *ContainerdRuntime) finishRunningCondition(ctx context.Context, cond *agentv1.ServiceCondition, svc *agentv1.DesiredService, status serviceStatus, created bool) *agentv1.ServiceCondition {
	cond.AppliedSpecRevision = status.AppliedSpecRevision
	cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
	cond.AllocationIpv4 = status.AllocationIPv4
	cond.AllocationIpv6 = status.AllocationIPv6
	check := explicitHTTPHealthCheck(svc)
	if check == nil {
		cond.Healthy = true
		cond.HealthyIpv4Ports = runtimeutil.ReadinessPorts(svc)
		cond.HealthyIpv6Ports = runtimeutil.ReadinessPorts(svc)
		cond.Phase = "Healthy"
		cond.Message = "process running; no health check configured"
		return cond
	}
	if created {
		delete(r.ready, svc.GetAllocationId())
	}
	if ready, ok := r.ready[svc.GetAllocationId()]; ok && ready.rolloutGeneration == svc.GetDesiredRolloutGeneration() {
		cond.Healthy = true
		cond.HealthyIpv4Ports = append([]int32(nil), ready.healthyIPv4Ports...)
		cond.HealthyIpv6Ports = append([]int32(nil), ready.healthyIPv6Ports...)
		cond.Phase = "Healthy"
		cond.Message = "HTTP readiness check passed"
		return cond
	}
	probeHealth := r.probeHealth
	if probeHealth == nil {
		probeHealth = probeServiceHealthInNamespace
	}
	ipv4Probe := probeHealth(ctx, status.NetworkNamespacePath, status.AllocationIPv4, svc)
	ipv6Probe := probeHealth(ctx, status.NetworkNamespacePath, status.AllocationIPv6, svc)
	if ipv4Probe.healthy || ipv6Probe.healthy {
		ports := runtimeutil.ReadinessPorts(svc)
		if r.ready == nil {
			r.ready = make(map[string]rolloutReadiness)
		}
		r.ready[svc.GetAllocationId()] = rolloutReadiness{
			rolloutGeneration: svc.GetDesiredRolloutGeneration(),
			healthyIPv4Ports:  healthyFamilyPorts(ipv4Probe.healthy, ports),
			healthyIPv6Ports:  healthyFamilyPorts(ipv6Probe.healthy, ports),
		}
		cond.Healthy = true
		cond.HealthyIpv4Ports = healthyFamilyPorts(ipv4Probe.healthy, ports)
		cond.HealthyIpv6Ports = healthyFamilyPorts(ipv6Probe.healthy, ports)
		cond.Phase = "Healthy"
		cond.Message = "HTTP readiness check passed over " + healthyFamilyLabel(ipv4Probe.healthy, ipv6Probe.healthy)
	} else {
		cond.Phase = "Starting"
		cond.Message = fmt.Sprintf("HTTP readiness check not ready: IPv4: %s; IPv6: %s", ipv4Probe.failureReason, ipv6Probe.failureReason)
	}
	return cond
}

func (r *ContainerdRuntime) livenessFailed(ctx context.Context, status serviceStatus, svc *agentv1.DesiredService) (string, bool) {
	check := explicitHTTPLivenessCheck(svc)
	if check == nil {
		return "", false
	}
	ready, ok := r.ready[svc.GetAllocationId()]
	if !ok || ready.rolloutGeneration != svc.GetDesiredRolloutGeneration() {
		if explicitHTTPHealthCheck(svc) != nil {
			return "", false
		}
	}
	probeHealth := r.probeHealth
	if probeHealth == nil {
		probeHealth = probeServiceHealthInNamespace
	}
	livenessSvc := protoCloneDesiredWithHealth(svc, check)
	ipv4Probe := probeHealth(ctx, status.NetworkNamespacePath, status.AllocationIPv4, livenessSvc)
	ipv6Probe := probeHealth(ctx, status.NetworkNamespacePath, status.AllocationIPv6, livenessSvc)
	if ipv4Probe.healthy || ipv6Probe.healthy {
		return "", false
	}
	reason := fmt.Sprintf("IPv4: %s; IPv6: %s", ipv4Probe.failureReason, ipv6Probe.failureReason)
	return reason, true
}

// inspectPreservedService resolves the status of an allocation that must stay
// stopped, without starting a container. Missing or stale containers report
// stopped from the saved observation; a live container keeps its real status.
func (r *ContainerdRuntime) inspectPreservedService(ctx context.Context, svc *agentv1.DesiredService, obs *platformv1.RestartObservation) (serviceStatus, error) {
	status, exists, err := r.engine.InspectService(ctx, svc)
	if err != nil {
		return serviceStatus{}, err
	}
	if exists {
		return status, nil
	}
	if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
		return serviceStatus{}, err
	}
	return serviceStatus{
		AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
		AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		AllocationIPv4:           svc.GetPrivateIpv4(),
		AllocationIPv6:           svc.GetPrivateIpv6(),
		Running:                  false,
		ExitCode:                 obs.GetLastExitCode(),
		Signal:                   obs.GetLastSignal(),
		OOMKilled:                obs.GetLastCause() == platformv1.RestartCause_RESTART_CAUSE_OOM_KILL,
		DiskExhausted:            obs.GetLastCause() == platformv1.RestartCause_RESTART_CAUSE_DISK_EXHAUSTED,
	}, nil
}

func shouldPreserveTerminalObservation(now time.Time, restart *platformv1.ServiceRestart, obs *platformv1.RestartObservation, desiredGeneration, operatorNonce int64) bool {
	if obs == nil {
		return false
	}
	if desiredGeneration > obs.GetAppliedRolloutGeneration() || operatorNonce > obs.GetAppliedOperatorRestartNonce() {
		return false
	}
	if obs.GetCrashLoop() {
		return true
	}
	if next := obs.GetNextRestartAt(); obs.GetAwaitingRestart() && next != nil && next.IsValid() {
		if now.Before(next.AsTime()) {
			return true
		}
	}
	if obs.GetLastCause() != platformv1.RestartCause_RESTART_CAUSE_UNSPECIFIED {
		policy := restartpolicy.CanonicalRestart(restart).GetPolicy()
		if !restartpolicy.ShouldRestart(policy, obs.GetLastCause()) {
			return true
		}
	}
	return false
}

func healthyFamilyPorts(healthy bool, ports []int32) []int32 {
	if !healthy {
		return nil
	}
	return append([]int32(nil), ports...)
}

func healthyFamilyLabel(ipv4, ipv6 bool) string {
	switch {
	case ipv4 && ipv6:
		return "IPv4 and IPv6"
	case ipv4:
		return "IPv4"
	default:
		return "IPv6"
	}
}

func protoCloneDesiredWithHealth(svc *agentv1.DesiredService, check *platformv1.HealthCheck) *agentv1.DesiredService {
	clone := &agentv1.DesiredService{
		AllocationId: svc.GetAllocationId(),
		ServiceId:    svc.GetServiceId(),
		Spec: &platformv1.ResolvedServiceSpec{
			Image: svc.GetSpec().GetImage(),
			Runtime: &platformv1.ServiceRuntime{
				Ports:       svc.GetSpec().GetRuntime().GetPorts(),
				HealthCheck: check,
			},
		},
	}
	return clone
}

func explicitHTTPLivenessCheck(svc *agentv1.DesiredService) *platformv1.HealthCheck {
	check := svc.GetSpec().GetRuntime().GetLivenessCheck()
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return nil
	}
	return check
}

func (r *ContainerdRuntime) pruneStaleServices(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	staleCandidates := make(map[string]struct{})
	resources, err := r.engine.DiscoverServices(ctx)
	if err != nil {
		return err
	}
	for _, resource := range resources {
		if resource.AllocationID != "" {
			staleCandidates[resource.AllocationID] = struct{}{}
		}
	}

	paths, err := filepath.Glob(filepath.Join(r.cfg.Runtime.DataDir, "desired", "*.json"))
	if err != nil {
		return fmt.Errorf("glob desired files: %w", err)
	}
	for _, path := range paths {
		allocationID := strings.TrimSuffix(filepath.Base(path), ".json")
		staleCandidates[allocationID] = struct{}{}
	}
	for allocationID := range staleCandidates {
		if _, ok := desired[allocationID]; ok {
			continue
		}
		if err := r.engine.RemoveService(ctx, allocationID); err != nil {
			return err
		}
		delete(r.ready, allocationID)
		if err := r.removeObservation(allocationID); err != nil {
			return err
		}
		path, err := runtimeChildPath(filepath.Join(r.cfg.Runtime.DataDir, "desired"), "allocation ID", allocationID)
		if err != nil {
			return err
		}
		if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove desired file %s: %w", path+".json", err)
		}
	}
	return nil
}

// checkVolumeAvailable refuses to start a workload whose volume is not pinned
// to this node or not provisioned: starting it would hand the workload an
// empty directory in place of its data.
func (r *ContainerdRuntime) checkVolumeAvailable(svc *agentv1.DesiredService, pinned map[string]bool) error {
	volumeID := svc.GetVolumeId()
	if volumeID == "" {
		return nil
	}
	if !pinned[volumeID] {
		return fmt.Errorf("volume %s is not pinned to this node", volumeID)
	}
	if _, err := runtimeutil.VolumeMountPath(svc.GetSpec().GetRuntime()); err != nil {
		return err
	}
	volumes, err := r.volumeStore()
	if err != nil {
		return err
	}
	if _, err := volumes.DataPath(volumeID); err != nil {
		return fmt.Errorf("volume %s is not available: %w", volumeID, err)
	}
	return nil
}

func (r *ContainerdRuntime) persistDesiredService(svc *agentv1.DesiredService) error {
	path, err := runtimeChildPath(filepath.Join(r.cfg.Runtime.DataDir, "desired"), "allocation ID", svc.GetAllocationId())
	if err != nil {
		return err
	}
	path += ".json"
	marker := fmt.Sprintf("{\"allocation_id\":%q}\n", svc.GetAllocationId())
	current, err := os.ReadFile(path)
	if err == nil && string(current) == marker {
		return os.Chmod(path, 0o600)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read desired service %s: %w", svc.GetAllocationId(), err)
	}
	if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

type serviceHealthProbe struct {
	configured    bool
	healthy       bool
	failureReason string
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func probeServiceHealthWithDialer(ctx context.Context, allocationIP string, svc *agentv1.DesiredService, dial dialContextFunc) serviceHealthProbe {
	runtime := svc.GetSpec().GetRuntime()
	check := runtime.GetHealthCheck()
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return serviceHealthProbe{}
	}
	result := serviceHealthProbe{configured: true}
	if check.GetType() != platformv1.HealthCheck_TYPE_HTTP {
		result.failureReason = "only HTTP readiness checks are supported"
		return result
	}
	if allocationIP == "" {
		result.failureReason = "allocation IP is unavailable"
		return result
	}
	port := runtimeutil.ReadinessCheckPort(runtime, check)
	if port == 0 {
		result.failureReason = "HTTP readiness check port is unavailable"
		return result
	}
	if err := probeHealthCheck(ctx, allocationIP, port, check, dial); err != nil {
		result.failureReason = err.Error()
		return result
	}
	result.healthy = true
	return result
}

func probeHealthCheck(ctx context.Context, allocationIP string, port int32, check *platformv1.HealthCheck, dial dialContextFunc) error {
	endpoint := net.JoinHostPort(allocationIP, fmt.Sprintf("%d", port))
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_HTTP:
		path := check.GetPath()
		if !validHealthCheckPath(path) {
			return fmt.Errorf("HTTP port %d: invalid health path %q", port, path)
		}
		client := healthHTTPClient(time.Duration(maxInt32(check.GetTimeoutSeconds(), 2))*time.Second, dial)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+path, nil)
		if err != nil {
			return fmt.Errorf("HTTP port %d: %w", port, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("HTTP port %d: %w", port, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP port %d: status %d", port, resp.StatusCode)
		}
		return nil
	}
	return errors.New("unsupported health check type")
}

func explicitHTTPHealthCheck(svc *agentv1.DesiredService) *platformv1.HealthCheck {
	check := svc.GetSpec().GetRuntime().GetHealthCheck()
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return nil
	}
	return check
}

func healthHTTPClient(timeout time.Duration, dial dialContextFunc) *http.Client {
	transport := &http.Transport{Proxy: nil}
	if dial != nil {
		transport.DialContext = dial
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func validHealthCheckPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.ContainsAny(path, "\r\n")
}

func containerName(allocationID string) string {
	return "platform-" + allocationID
}

func maxInt32(v int32, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}

func cpuCFSForMillis(cpuMillis int64) (int64, uint64) {
	if cpuMillis <= 0 {
		return 0, defaultCPUCFSPeriod
	}
	return cpuMillis * int64(defaultCPUCFSPeriod) / 1000, defaultCPUCFSPeriod
}

func (r *ContainerdRuntime) RecoveryResources(ctx context.Context) ([]recovery.FleetResource, error) {
	volumes, err := r.volumeStore()
	if err != nil {
		return nil, err
	}
	ids, err := volumes.Inventory()
	if err != nil {
		return nil, err
	}
	var resources []recovery.FleetResource
	for _, id := range ids {
		resources = append(resources, recovery.FleetResource{Kind: "volume", ID: id})
	}
	return resources, nil
}
