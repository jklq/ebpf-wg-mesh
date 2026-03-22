package localteststack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	localRuntimeVolumeMount = "/data"
	localRuntimeManagedBy   = "localteststack"
)

type DockerRuntimeConfig struct {
	DataDir            string
	VolumesDir         string
	DockerNetwork      string
	ContainerNamePrefx string
	Runner             DockerRunner
}

type DockerRuntime struct {
	cfg    DockerRuntimeConfig
	runner DockerRunner
}

type dockerContainerInspect struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func NewDockerRuntime(cfg DockerRuntimeConfig) (*DockerRuntime, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, fmt.Errorf("docker runtime data dir is required")
	}
	if strings.TrimSpace(cfg.VolumesDir) == "" {
		cfg.VolumesDir = filepath.Join(cfg.DataDir, "volumes")
	}
	if strings.TrimSpace(cfg.DockerNetwork) == "" {
		return nil, fmt.Errorf("docker runtime docker network is required")
	}
	if cfg.ContainerNamePrefx == "" {
		cfg.ContainerNamePrefx = "localteststack-svc"
	}
	runner := cfg.Runner
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "desired"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir desired dir: %w", err)
	}
	if err := os.MkdirAll(cfg.VolumesDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir volumes dir: %w", err)
	}
	if err := EnsureDockerNetwork(context.Background(), runner, cfg.DockerNetwork); err != nil {
		return nil, err
	}
	return &DockerRuntime{cfg: cfg, runner: runner}, nil
}

func (r *DockerRuntime) Close() error {
	if r == nil {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(r.cfg.DataDir, "desired", "*.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		allocationID := strings.TrimSuffix(filepath.Base(path), ".json")
		if err := r.removeService(context.Background(), allocationID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *DockerRuntime) Reconcile(ctx context.Context, state *agentv1.DesiredNodeState) (*agentv1.StatusReport, error) {
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
		path := filepath.Join(r.cfg.VolumesDir, vol.GetVolumeId())
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
		if err := r.persistDesiredService(svc); err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		status, created, healthy, err := r.ensureService(ctx, svc)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		cond.AppliedSpecRevision = status.AppliedSpecRevision
		cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
		cond.EndpointAddr = status.EndpointAddr
		cond.Healthy = healthy
		switch {
		case healthy:
			cond.Phase = "Healthy"
			cond.Message = "container healthy"
		case created:
			cond.Phase = "Starting"
			cond.Message = "container created or replaced"
		default:
			cond.Phase = "Running"
			cond.Message = "container reconciled"
		}
		report.Services = append(report.Services, cond)
	}
	return report, nil
}

type dockerServiceStatus struct {
	AppliedSpecRevision      int64
	AppliedRolloutGeneration int64
	EndpointAddr             string
}

func (r *DockerRuntime) ensureService(ctx context.Context, svc *agentv1.DesiredService) (dockerServiceStatus, bool, bool, error) {
	containerName := r.containerName(svc.GetAllocationId())
	if inspect, exists, err := r.inspectContainer(ctx, containerName); err != nil {
		return dockerServiceStatus{}, false, false, err
	} else if exists {
		if labelsMatchDesired(inspect.Config.Labels, svc) && inspect.State.Running {
			healthy := probeDockerHealth(inspect, svc)
			return dockerServiceStatus{
				AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
				AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
				EndpointAddr:             dockerRuntimeEndpoint(containerName, svc.GetSpec().GetRuntime()),
			}, false, healthy, nil
		}
		if err := r.removeService(ctx, svc.GetAllocationId()); err != nil {
			return dockerServiceStatus{}, false, false, err
		}
	}

	image := strings.TrimSpace(svc.GetSpec().GetImage())
	if image == "" {
		return dockerServiceStatus{}, false, false, fmt.Errorf("service image is required")
	}
	if err := r.ensureImage(ctx, image); err != nil {
		return dockerServiceStatus{}, false, false, err
	}

	args, err := r.dockerRunArgs(svc)
	if err != nil {
		return dockerServiceStatus{}, false, false, err
	}
	if _, err := r.runner.Run(ctx, args...); err != nil {
		return dockerServiceStatus{}, false, false, err
	}
	inspect, exists, err := r.inspectContainer(ctx, containerName)
	if err != nil {
		return dockerServiceStatus{}, false, false, err
	}
	if !exists {
		return dockerServiceStatus{}, false, false, fmt.Errorf("docker container %s did not start", containerName)
	}
	healthy := probeDockerHealth(inspect, svc)
	return dockerServiceStatus{
		AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
		AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		EndpointAddr:             dockerRuntimeEndpoint(containerName, svc.GetSpec().GetRuntime()),
	}, true, healthy, nil
}

func (r *DockerRuntime) ensureImage(ctx context.Context, image string) error {
	if _, err := r.runner.Run(ctx, "image", "inspect", image); err == nil {
		return nil
	}
	_, err := r.runner.Run(ctx, "pull", image)
	return err
}

func (r *DockerRuntime) dockerRunArgs(svc *agentv1.DesiredService) ([]string, error) {
	runtime := svc.GetSpec().GetRuntime()
	if runtime == nil {
		return nil, fmt.Errorf("service runtime is required")
	}
	args := []string{
		"run", "--detach", "--rm",
		"--name", r.containerName(svc.GetAllocationId()),
		"--network", r.cfg.DockerNetwork,
		"--hostname", svc.GetName(),
		"--label", "platform.managed=true",
		"--label", "platform.runtime=" + localRuntimeManagedBy,
		"--label", "platform.allocation_id=" + svc.GetAllocationId(),
		"--label", "platform.service_id=" + svc.GetServiceId(),
		"--label", "platform.desired_spec_revision=" + strconv.FormatInt(svc.GetDesiredSpecRevision(), 10),
		"--label", "platform.desired_rollout_generation=" + strconv.FormatInt(svc.GetDesiredRolloutGeneration(), 10),
	}
	for _, port := range publishedPorts(runtime) {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", port))
	}
	if svc.GetVolumeId() != "" {
		hostPath := filepath.Join(r.cfg.VolumesDir, svc.GetVolumeId())
		args = append(args, "--mount", "type=bind,src="+hostPath+",dst="+localRuntimeVolumeMount)
		args = append(args, "--env", "PLATFORM_VOLUME_DIR="+localRuntimeVolumeMount)
	}
	if runtime.GetCpuMillis() > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.3f", float64(runtime.GetCpuMillis())/1000.0))
	}
	if runtime.GetMemoryMebibytes() > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", runtime.GetMemoryMebibytes()))
	}
	for _, key := range sortedEnvKeys(runtime.GetEnv()) {
		args = append(args, "--env", key+"="+runtime.GetEnv()[key])
	}
	args = append(args, strings.TrimSpace(svc.GetSpec().GetImage()))
	if cmd := runtime.GetCommand(); len(cmd) > 0 {
		args = append(args, append([]string{}, cmd...)...)
		args = append(args, runtime.GetArgs()...)
	} else if len(runtime.GetArgs()) > 0 {
		args = append(args, runtime.GetArgs()...)
	}
	return args, nil
}

func (r *DockerRuntime) inspectContainer(ctx context.Context, name string) (dockerContainerInspect, bool, error) {
	raw, err := r.runner.Run(ctx, "inspect", name)
	if err != nil {
		if isDockerMissingObjectError(err) {
			return dockerContainerInspect{}, false, nil
		}
		return dockerContainerInspect{}, false, err
	}
	var decoded []dockerContainerInspect
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return dockerContainerInspect{}, false, fmt.Errorf("decode docker inspect %s: %w", name, err)
	}
	if len(decoded) == 0 {
		return dockerContainerInspect{}, false, nil
	}
	return decoded[0], true, nil
}

func (r *DockerRuntime) removeService(ctx context.Context, allocationID string) error {
	return removeContainer(ctx, r.runner, r.containerName(allocationID))
}

func (r *DockerRuntime) containerName(allocationID string) string {
	return r.cfg.ContainerNamePrefx + "-" + allocationID
}

func (r *DockerRuntime) pruneStaleServices(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	paths, err := filepath.Glob(filepath.Join(r.cfg.DataDir, "desired", "*.json"))
	if err != nil {
		return fmt.Errorf("glob desired files: %w", err)
	}
	for _, path := range paths {
		allocationID := strings.TrimSuffix(filepath.Base(path), ".json")
		if _, ok := desired[allocationID]; ok {
			continue
		}
		if err := r.removeService(ctx, allocationID); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove desired file %s: %w", path, err)
		}
	}
	return nil
}

func (r *DockerRuntime) pruneStaleVolumes(desired map[string]*agentv1.DesiredVolume) error {
	entries, err := os.ReadDir(r.cfg.VolumesDir)
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
		if err := os.RemoveAll(filepath.Join(r.cfg.VolumesDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale volume %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (r *DockerRuntime) persistDesiredService(svc *agentv1.DesiredService) error {
	path := filepath.Join(r.cfg.DataDir, "desired", svc.GetAllocationId()+".json")
	return os.WriteFile(path, []byte(protojson.Format(svc)), 0o644)
}

func dockerRuntimeEndpoint(containerName string, runtime *platformv1.ServiceRuntime) string {
	if runtime == nil || runtime.GetContainerPort() == 0 {
		return ""
	}
	return net.JoinHostPort(containerName, fmt.Sprintf("%d", runtime.GetContainerPort()))
}

func probeDockerHealth(inspect dockerContainerInspect, svc *agentv1.DesiredService) bool {
	runtime := svc.GetSpec().GetRuntime()
	if runtime == nil || runtime.GetContainerPort() == 0 {
		return false
	}
	check := runtime.GetHealthCheck()
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return probeTCP(resolvePublishedHostPort(inspect, runtime.GetContainerPort()), 2*time.Second)
	}
	port := check.GetPort()
	if port == 0 {
		port = runtime.GetContainerPort()
	}
	timeout := time.Duration(maxInt32(check.GetTimeoutSeconds(), 2)) * time.Second
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_HTTP:
		hostPort := resolvePublishedHostPort(inspect, port)
		if hostPort == "" {
			return false
		}
		client := http.Client{Timeout: timeout}
		resp, err := client.Get("http://" + hostPort + check.GetPath())
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
	case platformv1.HealthCheck_TYPE_TCP:
		return probeTCP(resolvePublishedHostPort(inspect, port), timeout)
	}
	return false
}

func probeTCP(endpoint string, timeout time.Duration) bool {
	if endpoint == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", endpoint, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func resolvePublishedHostPort(inspect dockerContainerInspect, containerPort int32) string {
	key := fmt.Sprintf("%d/tcp", containerPort)
	bindings := inspect.NetworkSettings.Ports[key]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return ""
	}
	hostIP := strings.TrimSpace(bindings[0].HostIP)
	if hostIP == "" || hostIP == "0.0.0.0" {
		hostIP = "127.0.0.1"
	}
	return net.JoinHostPort(hostIP, bindings[0].HostPort)
}

func publishedPorts(runtime *platformv1.ServiceRuntime) []int32 {
	if runtime == nil {
		return nil
	}
	seen := map[int32]struct{}{}
	var ports []int32
	if runtime.GetContainerPort() > 0 {
		seen[runtime.GetContainerPort()] = struct{}{}
		ports = append(ports, runtime.GetContainerPort())
	}
	if check := runtime.GetHealthCheck(); check != nil && check.GetPort() > 0 {
		if _, ok := seen[check.GetPort()]; !ok {
			ports = append(ports, check.GetPort())
		}
	}
	slices.Sort(ports)
	return ports
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func labelsMatchDesired(labels map[string]string, svc *agentv1.DesiredService) bool {
	if labels == nil {
		return false
	}
	return labels["platform.runtime"] == localRuntimeManagedBy &&
		labels["platform.allocation_id"] == svc.GetAllocationId() &&
		labels["platform.service_id"] == svc.GetServiceId() &&
		labels["platform.desired_spec_revision"] == strconv.FormatInt(svc.GetDesiredSpecRevision(), 10) &&
		labels["platform.desired_rollout_generation"] == strconv.FormatInt(svc.GetDesiredRolloutGeneration(), 10)
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

func maxInt32(v int32, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}
