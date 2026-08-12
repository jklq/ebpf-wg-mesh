package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

const defaultVolumeMount = "/data"

const defaultCPUCFSPeriod uint64 = 100_000

var jsonEncoderPool = sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

type serviceEngine interface {
	EnsureService(context.Context, *agentv1.DesiredService) (serviceStatus, bool, error)
	RemoveService(context.Context, string) error
	SetLogSink(LogSink)
	Close() error
}

type serviceStatus struct {
	AppliedSpecRevision      int64
	AppliedRolloutGeneration int64
	AllocationIP             string
	NetworkNamespacePath     string
}

type ContainerdRuntime struct {
	cfg         config.AgentConfig
	engine      serviceEngine
	probeHealth func(context.Context, string, string, *agentv1.DesiredService) serviceHealthProbe
	ready       map[string]rolloutReadiness
}

type rolloutReadiness struct {
	rolloutGeneration int64
	healthyPorts      []int32
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
	return &ContainerdRuntime{cfg: cfg, engine: engine}, nil
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
		if err := r.engine.RemoveService(ctx, svc.GetAllocationId()); err != nil {
			return fmt.Errorf("restart managed dashboard: %w", err)
		}
	}
	return nil
}

func (r *ContainerdRuntime) Reconcile(ctx context.Context, state *agentv1.DesiredNodeState) (*agentv1.StatusReport, error) {
	report := &agentv1.StatusReport{AgentId: state.GetAgentId()}
	desiredVolumes := indexDesiredVolumes(state.GetVolumes())
	desiredServices := indexDesiredServices(state.GetServices())

	if err := r.pruneStaleServices(ctx, desiredServices); err != nil {
		return nil, err
	}
	if err := r.pruneStaleVolumes(desiredVolumes); err != nil {
		return nil, err
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
		cond := &agentv1.ServiceCondition{
			AllocationId:             svc.GetAllocationId(),
			ServiceId:                svc.GetServiceId(),
			DesiredSpecRevision:      svc.GetDesiredSpecRevision(),
			DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(),
			Phase:                    "Pending",
		}
		if err := validateRuntimeID("allocation ID", svc.GetAllocationId()); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		if volumeID := svc.GetVolumeId(); volumeID != "" {
			if _, err := runtimeChildPath(r.cfg.Runtime.VolumesDir, "volume ID", volumeID); err != nil {
				cond.Phase = "Error"
				cond.Message = err.Error()
				report.Services = append(report.Services, cond)
				continue
			}
		}
		if err := r.persistDesiredService(svc); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		status, created, err := r.engine.EnsureService(ctx, svc)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		cond.AppliedSpecRevision = status.AppliedSpecRevision
		cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
		cond.AllocationIp = status.AllocationIP
		check := explicitHTTPHealthCheck(svc)
		if check == nil {
			cond.Healthy = true
			cond.HealthyPorts = readinessPorts(svc)
			cond.Phase = "Healthy"
			cond.Message = "process running; no health check configured"
			report.Services = append(report.Services, cond)
			continue
		}
		if created {
			delete(r.ready, svc.GetAllocationId())
		}
		if ready, ok := r.ready[svc.GetAllocationId()]; ok && ready.rolloutGeneration == svc.GetDesiredRolloutGeneration() {
			cond.Healthy = true
			cond.HealthyPorts = append([]int32(nil), ready.healthyPorts...)
			cond.Phase = "Healthy"
			cond.Message = "HTTP readiness check passed"
			report.Services = append(report.Services, cond)
			continue
		}
		probeHealth := r.probeHealth
		if probeHealth == nil {
			probeHealth = probeServiceHealthInNamespace
		}
		probe := probeHealth(ctx, status.NetworkNamespacePath, status.AllocationIP, svc)
		if probe.healthy {
			ports := readinessPorts(svc)
			if r.ready == nil {
				r.ready = make(map[string]rolloutReadiness)
			}
			r.ready[svc.GetAllocationId()] = rolloutReadiness{
				rolloutGeneration: svc.GetDesiredRolloutGeneration(),
				healthyPorts:      append([]int32(nil), ports...),
			}
			cond.Healthy = true
			cond.HealthyPorts = ports
			cond.Phase = "Healthy"
			cond.Message = "HTTP readiness check passed"
		} else {
			cond.Phase = "Starting"
			cond.Message = "HTTP readiness check not ready: " + probe.failureReason
		}
		report.Services = append(report.Services, cond)
	}
	return report, nil
}

func (r *ContainerdRuntime) pruneStaleServices(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	paths, err := filepath.Glob(filepath.Join(r.cfg.Runtime.DataDir, "desired", "*.json"))
	if err != nil {
		return fmt.Errorf("glob desired files: %w", err)
	}
	for _, path := range paths {
		allocationID := strings.TrimSuffix(filepath.Base(path), ".json")
		if _, ok := desired[allocationID]; ok {
			continue
		}
		if err := r.engine.RemoveService(ctx, allocationID); err != nil {
			return err
		}
		delete(r.ready, allocationID)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove desired file %s: %w", path, err)
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

func probeServiceHealth(allocationIP string, svc *agentv1.DesiredService) serviceHealthProbe {
	return probeServiceHealthWithDialer(context.Background(), allocationIP, svc, (&net.Dialer{}).DialContext)
}

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
	port := readinessCheckPort(runtime, check)
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

func readinessCheckPort(runtime *platformv1.ServiceRuntime, check *platformv1.HealthCheck) int32 {
	if check.GetPort() > 0 {
		return check.GetPort()
	}
	for _, item := range runtime.GetPorts() {
		if item.GetPrimary() {
			return item.GetPort()
		}
	}
	ports := runtimePortNumbers(runtime)
	if len(ports) > 0 {
		return ports[0]
	}
	return 0
}

func readinessPorts(svc *agentv1.DesiredService) []int32 {
	runtime := svc.GetSpec().GetRuntime()
	ports := runtimePortNumbers(runtime)
	if len(ports) == 0 {
		if check := runtime.GetHealthCheck(); check != nil && check.GetPort() > 0 {
			return []int32{check.GetPort()}
		}
	}
	return ports
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

func runtimePortNumbers(runtime *platformv1.ServiceRuntime) []int32 {
	if runtime == nil {
		return nil
	}
	seen := make(map[int32]struct{}, len(runtime.GetPorts()))
	var out []int32
	for _, item := range runtime.GetPorts() {
		port := item.GetPort()
		if port < 1 || port > 65535 {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		out = append(out, port)
	}
	return out
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

func indexDesiredVolumes(items []*agentv1.DesiredVolume) map[string]*agentv1.DesiredVolume {
	out := make(map[string]*agentv1.DesiredVolume, len(items))
	for _, item := range items {
		out[item.GetVolumeId()] = item
	}
	return out
}

func indexDesiredServices(items []*agentv1.DesiredService) map[string]*agentv1.DesiredService {
	out := make(map[string]*agentv1.DesiredService, len(items))
	for _, item := range items {
		out[item.GetAllocationId()] = item
	}
	return out
}
