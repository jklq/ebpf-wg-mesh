package localteststack

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"ebof-wg-mesh/internal/meshlabels"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
)

const (
	localRuntimeVolumeMount = "/data"
	localRuntimeManagedBy   = "localteststack"
	internalDomainSuffix    = ".mesh.internal"
	internalHostnameLabel   = "platform.internal_hostname"
	// localRuntimeLabel marks containers this runtime owns; the real agent
	// never writes it.
	localRuntimeLabel = "platform.runtime"
)

type DockerRuntimeConfig struct {
	DataDir            string
	VolumesDir         string
	DockerNetwork      string
	ContainerNamePrefx string
	Runner             DockerRunner
}

type DockerRuntime struct {
	cfg                 DockerRuntimeConfig
	runner              DockerRunner
	ready               map[string]dockerRolloutReadiness
	environmentNetworks map[string]struct{}
}

type dockerRolloutReadiness struct {
	rolloutGeneration int64
	healthyPorts      []int32
}

type dockerContainerState struct {
	Running   bool   `json:"Running"`
	ExitCode  int    `json:"ExitCode"`
	OOMKilled bool   `json:"OOMKilled"`
	Error     string `json:"Error"`
}

type dockerContainerInspect struct {
	State  dockerContainerState `json:"State"`
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
	return &DockerRuntime{
		cfg:                 cfg,
		runner:              runner,
		environmentNetworks: make(map[string]struct{}),
	}, nil
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
	for network := range r.environmentNetworks {
		if err := RemoveDockerNetwork(context.Background(), r.runner, network); err != nil {
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
		path, pathErr := safeRuntimeChildPath(r.cfg.VolumesDir, "volume ID", vol.GetVolumeId())
		if pathErr != nil {
			report.Volumes = append(report.Volumes, &agentv1.VolumeCondition{
				VolumeId: vol.GetVolumeId(), Phase: "Error", Message: pathErr.Error(),
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
		if err := validateRuntimeIdentifier("allocation ID", svc.GetAllocationId()); err != nil {
			cond.Phase, cond.Message = "Error", err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		if volumeID := svc.GetVolumeId(); volumeID != "" {
			if _, err := safeRuntimeChildPath(r.cfg.VolumesDir, "volume ID", volumeID); err != nil {
				cond.Phase, cond.Message = "Error", err.Error()
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
		if svc.GetIntent() == agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN {
			deadline := svc.GetDrainDeadline()
			if deadline == nil || !deadline.IsValid() {
				cond.Phase, cond.Message = "Error", "drain deadline is required"
				report.Services = append(report.Services, cond)
				continue
			}
			drained, forced, err := r.drainService(ctx, svc.GetAllocationId(), deadline.AsTime())
			if err != nil {
				cond.Phase, cond.Message = "Error", err.Error()
				report.Services = append(report.Services, cond)
				continue
			}
			cond.AppliedSpecRevision = svc.GetDesiredSpecRevision()
			cond.AppliedRolloutGeneration = svc.GetDesiredRolloutGeneration()
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
			report.Services = append(report.Services, cond)
			continue
		}
		status, created, err := r.ensureService(ctx, svc)
		if err != nil {
			cond.Phase = "Error"
			cond.Message = err.Error()
			report.Services = append(report.Services, cond)
			continue
		}
		if created {
			status.Running = true
			delete(r.ready, svc.GetAllocationId())
		}
		decision := restartpolicy.Evaluate(time.Now().UTC(), nil, svc.GetSpec().GetRuntime().GetRestart(), svc.GetRestartObservation(), restartpolicy.Input{
			State: restartpolicy.ProcessState{
				Running:   status.Running,
				ExitCode:  int32(status.inspect.State.ExitCode),
				OOMKilled: status.inspect.State.OOMKilled,
			},
			DesiredRolloutGeneration: svc.GetDesiredRolloutGeneration(),
			OperatorRestartNonce:     svc.GetOperatorRestartNonce(),
		})
		cond.Restart = decision.Observation
		switch decision.Action {
		case restartpolicy.ActionCrashLoop, restartpolicy.ActionStop, restartpolicy.ActionWait:
			cond.AppliedSpecRevision = status.AppliedSpecRevision
			cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
			cond.AllocationIp = status.AllocationIP
			cond.Healthy = false
			cond.Phase = decision.Phase
			cond.Message = decision.Message
			report.Services = append(report.Services, cond)
			continue
		case restartpolicy.ActionStart:
			if !status.Running {
				if err := r.removeService(ctx, svc.GetAllocationId()); err != nil {
					cond.Phase, cond.Message = "Error", err.Error()
					report.Services = append(report.Services, cond)
					continue
				}
				status, created, err = r.ensureService(ctx, svc)
				if err != nil {
					cond.Phase, cond.Message = "Error", err.Error()
					report.Services = append(report.Services, cond)
					continue
				}
			}
		}
		cond.AppliedSpecRevision = status.AppliedSpecRevision
		cond.AppliedRolloutGeneration = status.AppliedRolloutGeneration
		cond.AllocationIp = status.AllocationIP
		check := svc.GetSpec().GetRuntime().GetHealthCheck()
		if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
			cond.Healthy = true
			cond.HealthyPorts = dockerReadinessPorts(svc)
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
		probe := probeDockerReadiness(status.inspect, svc)
		if probe.healthy {
			ports := dockerReadinessPorts(svc)
			if r.ready == nil {
				r.ready = make(map[string]dockerRolloutReadiness)
			}
			r.ready[svc.GetAllocationId()] = dockerRolloutReadiness{
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

type dockerServiceStatus struct {
	AppliedSpecRevision      int64
	AppliedRolloutGeneration int64
	AllocationIP             string
	Running                  bool
	inspect                  dockerContainerInspect
}

func (r *DockerRuntime) ensureService(ctx context.Context, svc *agentv1.DesiredService) (dockerServiceStatus, bool, error) {
	containerName := r.containerName(svc.GetAllocationId())
	environmentNetwork := r.environmentNetworkName(svc.GetEnvironmentId())
	if inspect, exists, err := r.inspectContainer(ctx, containerName); err != nil {
		return dockerServiceStatus{}, false, err
	} else if exists {
		if labelsMatchDesired(inspect.Config.Labels, svc) {
			return dockerServiceStatusFor(inspect, svc, r.cfg.DockerNetwork), false, nil
		}
		if err := r.removeService(ctx, svc.GetAllocationId()); err != nil {
			return dockerServiceStatus{}, false, err
		}
	}

	image := strings.TrimSpace(svc.GetSpec().GetImage())
	if image == "" {
		return dockerServiceStatus{}, false, fmt.Errorf("service image is required")
	}
	if err := r.ensureImage(ctx, image, svc.GetRegistryUsername(), svc.GetRegistryPassword()); err != nil {
		return dockerServiceStatus{}, false, err
	}
	if err := EnsureDockerNetwork(ctx, r.runner, environmentNetwork); err != nil {
		return dockerServiceStatus{}, false, err
	}
	if environmentNetwork != r.cfg.DockerNetwork {
		if r.environmentNetworks == nil {
			r.environmentNetworks = make(map[string]struct{})
		}
		r.environmentNetworks[environmentNetwork] = struct{}{}
	}

	args, err := r.dockerRunArgs(svc)
	if err != nil {
		return dockerServiceStatus{}, false, err
	}
	if _, err := r.runner.Run(ctx, args...); err != nil {
		return dockerServiceStatus{}, false, err
	}
	if environmentNetwork != r.cfg.DockerNetwork {
		if _, err := r.runner.Run(ctx, "network", "connect", r.cfg.DockerNetwork, containerName); err != nil {
			_ = r.removeService(ctx, svc.GetAllocationId())
			return dockerServiceStatus{}, false, fmt.Errorf("connect %s to ingress network: %w", containerName, err)
		}
	}
	inspect, exists, err := r.inspectContainer(ctx, containerName)
	if err != nil {
		return dockerServiceStatus{}, false, err
	}
	if !exists {
		return dockerServiceStatus{}, false, fmt.Errorf("docker container %s did not start", containerName)
	}
	return dockerServiceStatusFor(inspect, svc, r.cfg.DockerNetwork), true, nil
}

func dockerServiceStatusFor(inspect dockerContainerInspect, svc *agentv1.DesiredService, networkName string) dockerServiceStatus {
	return dockerServiceStatus{
		AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
		AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		AllocationIP:             dockerAllocationIP(inspect, networkName),
		Running:                  inspect.State.Running,
		inspect:                  inspect,
	}
}

func (r *DockerRuntime) ensureImage(ctx context.Context, image, username, password string) error {
	if _, err := r.runner.Run(ctx, "image", "inspect", image); err == nil {
		return nil
	}
	if username == "" && password == "" {
		_, err := r.runner.Run(ctx, "pull", image)
		return err
	}
	host := strings.SplitN(image, "/", 2)[0]
	if host == "" || !strings.Contains(image, "/") {
		return fmt.Errorf("authenticated image reference must include a registry host")
	}
	configDir, err := os.MkdirTemp(r.cfg.DataDir, "registry-auth-")
	if err != nil {
		return fmt.Errorf("create registry auth directory: %w", err)
	}
	defer os.RemoveAll(configDir)
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	configBody, err := json.Marshal(map[string]any{
		"auths": map[string]any{host: map[string]string{"auth": auth}},
	})
	if err != nil {
		return fmt.Errorf("encode registry auth: %w", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), configBody, 0o600); err != nil {
		return fmt.Errorf("write registry auth: %w", err)
	}
	_, err = r.runner.Run(ctx, "--config", configDir, "pull", image)
	return err
}

func (r *DockerRuntime) dockerRunArgs(svc *agentv1.DesiredService) ([]string, error) {
	if err := validateRuntimeIdentifier("allocation ID", svc.GetAllocationId()); err != nil {
		return nil, err
	}
	runtime := svc.GetSpec().GetRuntime()
	if runtime == nil {
		return nil, fmt.Errorf("service runtime is required")
	}
	args := []string{
		"run", "--detach", "--rm",
		"--name", r.containerName(svc.GetAllocationId()),
		"--network", r.environmentNetworkName(svc.GetEnvironmentId()),
		"--hostname", svc.GetName(),
		"--label", meshlabels.Managed + "=true",
		"--label", localRuntimeLabel + "=" + localRuntimeManagedBy,
		"--label", meshlabels.AllocationID + "=" + svc.GetAllocationId(),
		"--label", meshlabels.ServiceID + "=" + svc.GetServiceId(),
		"--label", meshlabels.DesiredSpecRevision + "=" + strconv.FormatInt(svc.GetDesiredSpecRevision(), 10),
		"--label", meshlabels.DesiredRolloutGeneration + "=" + strconv.FormatInt(svc.GetDesiredRolloutGeneration(), 10),
		"--label", internalHostnameLabel + "=" + svc.GetInternalHostname(),
	}
	args = append(args, dockerSandboxArgs(runtime)...)
	if hostname := strings.TrimSpace(svc.GetInternalHostname()); hostname != "" {
		args = append(args, "--network-alias", hostname)
		if shortName := strings.TrimSuffix(hostname, internalDomainSuffix); shortName != hostname {
			args = append(args, "--network-alias", shortName)
		}
	}
	for _, port := range publishedPorts(runtime) {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", port))
	}
	if svc.GetVolumeId() != "" {
		hostPath, err := safeRuntimeChildPath(r.cfg.VolumesDir, "volume ID", svc.GetVolumeId())
		if err != nil {
			return nil, err
		}
		args = append(args, "--mount", "type=bind,src="+hostPath+",dst="+localRuntimeVolumeMount+",bind-propagation=rprivate")
		args = append(args, "--env", "PLATFORM_VOLUME_DIR="+localRuntimeVolumeMount)
	}
	if runtime.GetCpuMillis() > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%.3f", float64(runtime.GetCpuMillis())/1000.0))
	}
	if runtime.GetMemoryMebibytes() > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", runtime.GetMemoryMebibytes()))
		args = append(args, "--memory-swap", fmt.Sprintf("%dm", runtime.GetMemoryMebibytes()))
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

func dockerSandboxArgs(runtime *platformv1.ServiceRuntime) []string {
	allowRoot := false
	writableRootFS := false
	for _, relaxation := range runtime.GetSandboxProfile().GetRelaxations() {
		switch relaxation {
		case platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT:
			allowRoot = true
		case platformv1.SandboxRelaxation_SANDBOX_RELAXATION_WRITABLE_ROOT_FILESYSTEM:
			writableRootFS = true
		}
	}
	args := []string{
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--pids-limit", "256",
		"--oom-score-adj", "500",
	}
	if !allowRoot {
		args = append(args, "--user", "65532:65532")
	}
	if !writableRootFS {
		args = append(args,
			"--read-only",
			"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
			"--tmpfs", "/var/tmp:rw,noexec,nosuid,nodev,size=64m",
			"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=16m",
		)
	}
	return args
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

func (r *DockerRuntime) drainService(ctx context.Context, allocationID string, deadline time.Time) (bool, bool, error) {
	name := r.containerName(allocationID)
	inspect, exists, err := r.inspectContainer(ctx, name)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return true, false, nil
	}
	if inspect.State.Running && time.Now().UTC().Before(deadline.UTC()) {
		if _, err := r.runner.Run(ctx, "kill", "--signal", "TERM", name); err != nil && !isDockerMissingObjectError(err) {
			return false, false, err
		}
		return false, false, nil
	}
	forced := inspect.State.Running
	args := []string{"rm"}
	if forced {
		args = append(args, "--force")
	}
	args = append(args, name)
	if _, err := r.runner.Run(ctx, args...); err != nil && !isDockerMissingObjectError(err) {
		return false, forced, err
	}
	delete(r.ready, allocationID)
	return true, forced, nil
}

func (r *DockerRuntime) containerName(allocationID string) string {
	return r.cfg.ContainerNamePrefx + "-" + allocationID
}

func (r *DockerRuntime) environmentNetworkName(environmentID string) string {
	environmentID = strings.TrimSpace(environmentID)
	if environmentID == "" {
		return r.cfg.DockerNetwork
	}
	digest := sha256.Sum256([]byte(environmentID))
	return fmt.Sprintf("%s-env-%x", r.cfg.DockerNetwork, digest[:6])
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
		delete(r.ready, allocationID)
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
	path, err := safeRuntimeChildPath(filepath.Join(r.cfg.DataDir, "desired"), "allocation ID", svc.GetAllocationId())
	if err != nil {
		return err
	}
	path += ".json"
	if err := os.WriteFile(path, []byte(protojson.Format(svc)), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func validateRuntimeIdentifier(kind, id string) error {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("invalid %s %q", kind, id)
	}
	return nil
}

func safeRuntimeChildPath(base, kind, id string) (string, error) {
	if err := validateRuntimeIdentifier(kind, id); err != nil {
		return "", err
	}
	cleanBase, err := filepath.Abs(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("resolve %s base: %w", kind, err)
	}
	path := filepath.Clean(filepath.Join(cleanBase, id))
	rel, err := filepath.Rel(cleanBase, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q escapes runtime directory", kind, id)
	}
	return path, nil
}

func dockerAllocationIP(inspect dockerContainerInspect, networkName string) string {
	if inspect.NetworkSettings.Networks == nil {
		return ""
	}
	if networkName != "" {
		if network := inspect.NetworkSettings.Networks[networkName]; strings.TrimSpace(network.IPAddress) != "" {
			return strings.TrimSpace(network.IPAddress)
		}
	}
	for _, network := range inspect.NetworkSettings.Networks {
		if ip := strings.TrimSpace(network.IPAddress); ip != "" {
			return ip
		}
	}
	return ""
}

type dockerReadinessProbe struct {
	healthy       bool
	failureReason string
}

func probeDockerReadiness(inspect dockerContainerInspect, svc *agentv1.DesiredService) dockerReadinessProbe {
	runtime := svc.GetSpec().GetRuntime()
	check := runtime.GetHealthCheck()
	result := dockerReadinessProbe{}
	if check == nil || check.GetType() != platformv1.HealthCheck_TYPE_HTTP {
		result.failureReason = "only HTTP readiness checks are supported"
		return result
	}
	port := readinessCheckPort(runtime, check)
	if port == 0 {
		result.failureReason = "HTTP readiness check port is unavailable"
		return result
	}
	if err := probeDockerHealthCheck(inspect, port, check); err != nil {
		result.failureReason = err.Error()
		return result
	}
	result.healthy = true
	return result
}

func probeDockerHealthCheck(inspect dockerContainerInspect, port int32, check *platformv1.HealthCheck) error {
	timeout := time.Duration(maxInt32(check.GetTimeoutSeconds(), 2)) * time.Second
	switch check.GetType() {
	case platformv1.HealthCheck_TYPE_HTTP:
		hostPort := resolvePublishedHostPort(inspect, port)
		if hostPort == "" {
			return fmt.Errorf("HTTP port %d is not published", port)
		}
		path := check.GetPath()
		if !validHealthCheckPath(path) {
			return fmt.Errorf("HTTP port %d has invalid health path %q", port, path)
		}
		client := http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Get("http://" + hostPort + check.GetPath())
		if err != nil {
			return fmt.Errorf("HTTP port %d: %w", port, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP port %d: status %d", port, resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("unsupported health check type")
}

func dockerReadinessPorts(svc *agentv1.DesiredService) []int32 {
	runtime := svc.GetSpec().GetRuntime()
	ports := runtimePortNumbers(runtime)
	if len(ports) == 0 {
		if check := runtime.GetHealthCheck(); check != nil && check.GetPort() > 0 {
			return []int32{check.GetPort()}
		}
	}
	return ports
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

func validHealthCheckPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.ContainsAny(path, "\r\n")
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

func runtimePortNumbers(runtime *platformv1.ServiceRuntime) []int32 {
	if runtime == nil {
		return nil
	}
	seen := map[int32]struct{}{}
	var ports []int32
	for _, item := range runtime.GetPorts() {
		port := item.GetPort()
		if port < 1 || port > 65535 {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports
}

func publishedPorts(runtime *platformv1.ServiceRuntime) []int32 {
	if runtime == nil {
		return nil
	}
	seen := map[int32]struct{}{}
	var ports []int32
	for _, item := range runtime.GetPorts() {
		port := item.GetPort()
		if port < 1 || port > 65535 {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
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
	return labels[localRuntimeLabel] == localRuntimeManagedBy &&
		labels[meshlabels.AllocationID] == svc.GetAllocationId() &&
		labels[meshlabels.ServiceID] == svc.GetServiceId() &&
		labels[meshlabels.DesiredSpecRevision] == strconv.FormatInt(svc.GetDesiredSpecRevision(), 10) &&
		labels[meshlabels.DesiredRolloutGeneration] == strconv.FormatInt(svc.GetDesiredRolloutGeneration(), 10) &&
		labels[internalHostnameLabel] == svc.GetInternalHostname()
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
