package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
	"ebof-wg-mesh/internal/restartpolicy"
	"ebof-wg-mesh/internal/runtimeutil"
)

const defaultVolumeMount = "/data"

const defaultCPUCFSPeriod uint64 = 100_000

var jsonEncoderPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

type serviceEngine interface {
	DiscoverServices(context.Context) ([]RuntimeResource, error)
	EnsureService(context.Context, *agentv1.DesiredService) (serviceStatus, bool, error)
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
}

type ContainerdRuntime struct {
	cfg         config.AgentConfig
	engine      serviceEngine
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
	if err := os.MkdirAll(cfg.Runtime.VolumesDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir volumes dir: %w", err)
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
	engine, err := newContainerdEngine(cfg)
	if err != nil {
		return nil, err
	}
	return &ContainerdRuntime{
		cfg:    cfg,
		engine: engine,
		clock:  restartpolicy.SystemClock{},
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
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
	resources, err := r.engine.DiscoverServices(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(r.cfg.Runtime.VolumesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("discover runtime volumes: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path, err := runtimeChildPath(r.cfg.Runtime.VolumesDir, "volume ID", entry.Name())
		if err != nil {
			return nil, err
		}
		resources = append(resources, RuntimeResource{VolumeID: entry.Name(), RuntimeID: path})
	}
	return resources, nil
}

func (r *ContainerdRuntime) ReconcileWithCleanup(ctx context.Context, state *agentv1.DesiredNodeState, allowCleanup bool) (*agentv1.StatusReport, error) {
	report := &agentv1.StatusReport{AgentId: state.GetAgentId()}
	desiredVolumes := runtimeutil.IndexDesiredVolumes(state.GetVolumes())
	desiredServices := runtimeutil.IndexDesiredServices(state.GetServices())

	if allowCleanup {
		if err := r.pruneStaleServices(ctx, desiredServices); err != nil {
			return nil, err
		}
		if err := r.pruneStaleVolumes(desiredVolumes); err != nil {
			return nil, err
		}
	}

	for _, vol := range state.GetVolumes() {
		path, pathErr := runtimeChildPath(r.cfg.Runtime.VolumesDir, "volume ID", vol.GetVolumeId())
		if pathErr != nil {
			report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
				VolumeId: vol.GetVolumeId(),
				Phase:    "Error",
				Message:  pathErr.Error(),
			})
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
				VolumeId: vol.GetVolumeId(),
				Phase:    "Error",
				Message:  err.Error(),
			})
			continue
		}
		report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
			VolumeId: vol.GetVolumeId(),
			Phase:    "Ready",
			Message:  path,
		})
	}

	for _, svc := range state.GetServices() {
		cond := r.reconcileService(ctx, svc)
		report.Services = append(report.Services, cond)
	}
	return report, nil
}

func (r *ContainerdRuntime) reconcileService(ctx context.Context, svc *agentv1.DesiredService) *agentv1.ServiceCondition {
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
	if volumeID := svc.GetVolumeId(); volumeID != "" {
		if _, err := runtimeChildPath(r.cfg.Runtime.VolumesDir, "volume ID", volumeID); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			return cond
		}
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
	status, created, err := r.engine.EnsureService(ctx, svc)
	if err != nil {
		cond.Phase = "Error"
		cond.Message = err.Error()
		return cond
	}
	if created {
		status.Running = true
		delete(r.ready, svc.GetAllocationId())
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
	obs := r.loadObservation(svc.GetAllocationId(), svc.GetRestartObservation())
	operatorNonce := svc.GetOperatorRestartNonce()
	if r.forceStart[svc.GetAllocationId()] {
		if obs.GetAppliedOperatorRestartNonce() >= operatorNonce {
			operatorNonce = obs.GetAppliedOperatorRestartNonce() + 1
		}
		delete(r.forceStart, svc.GetAllocationId())
	}
	r.rngMu.Lock()
	decision := restartpolicy.Evaluate(r.now(), r.randSource(), svc.GetSpec().GetRuntime().GetRestart(), obs, restartpolicy.Input{
		State: restartpolicy.ProcessState{
			Running:   status.Running,
			ExitCode:  status.ExitCode,
			Signal:    status.Signal,
			OOMKilled: status.OOMKilled,
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

func (r *ContainerdRuntime) pruneStaleVolumes(desired map[string]*agentv1.DesiredVolume) error {
	entries, err := os.ReadDir(r.cfg.Runtime.VolumesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read volumes dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := desired[entry.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.cfg.Runtime.VolumesDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale volume %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (r *ContainerdRuntime) persistDesiredService(svc *agentv1.DesiredService) error {
	path, err := runtimeChildPath(filepath.Join(r.cfg.Runtime.DataDir, "desired"), "allocation ID", svc.GetAllocationId())
	if err != nil {
		return err
	}
	path += ".json"
	buf := jsonEncoderPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonEncoderPool.Put(buf)
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(svc); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, buf.Bytes()) {
		return os.Chmod(path, 0o600)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read desired service %s: %w", svc.GetAllocationId(), err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
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
