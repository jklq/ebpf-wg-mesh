package localteststack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/agent"
	"ebof-wg-mesh/internal/meshlabels"
	"ebof-wg-mesh/internal/restartpolicy"
	"ebof-wg-mesh/internal/runtimeutil"
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
	cfg    DockerRuntimeConfig
	runner DockerRunner
	ready  map[string]dockerRolloutReadiness
}

type dockerRolloutReadiness struct {
	rolloutGeneration int64
	healthyIPv4Ports  []int32
	healthyIPv6Ports  []int32
}

type dockerContainerState struct {
	Running   bool   `json:"Running"`
	ExitCode  int    `json:"ExitCode"`
	OOMKilled bool   `json:"OOMKilled"`
	Error     string `json:"Error"`
}

type dockerContainerInspect struct {
	ID     string               `json:"Id"`
	Name   string               `json:"Name"`
	State  dockerContainerState `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
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
		cfg:    cfg,
		runner: runner,
	}, nil
}

func (r *DockerRuntime) Close() error {
	return nil
}

func (r *DockerRuntime) Reconcile(ctx context.Context, state *agentv1.DesiredNodeState) (*agentv1.StatusReport, error) {
	return r.ReconcileWithCleanup(ctx, state, true)
}

func (r *DockerRuntime) DiscoverRuntimeResources(ctx context.Context) ([]agent.RuntimeResource, error) {
	if r == nil {
		return nil, nil
	}
	ids, err := listContainerIDs(ctx, r.runner, "label="+localRuntimeLabel+"="+localRuntimeManagedBy)
	if err != nil {
		return nil, fmt.Errorf("discover docker workloads: %w", err)
	}
	resources := make([]agent.RuntimeResource, 0, len(ids))
	seenAllocations := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		inspect, exists, err := r.inspectContainer(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect discovered docker workload %s: %w", id, err)
		}
		if !exists {
			continue
		}
		allocationID := strings.TrimSpace(inspect.Config.Labels[meshlabels.AllocationID])
		if err := validateRuntimeIdentifier("allocation ID", allocationID); err != nil {
			return nil, fmt.Errorf("discover docker workload %s: %w", id, err)
		}
		expectedName := r.containerName(allocationID)
		if actualName := strings.TrimPrefix(inspect.Name, "/"); actualName != "" && actualName != expectedName {
			return nil, fmt.Errorf("discover docker workload %s: runtime name %q does not match stable identity %q", id, actualName, expectedName)
		}
		if _, duplicate := seenAllocations[allocationID]; duplicate {
			return nil, fmt.Errorf("discover docker workload %s: duplicate allocation %q", id, allocationID)
		}
		seenAllocations[allocationID] = struct{}{}
		resources = append(resources, agent.RuntimeResource{AllocationID: allocationID, RuntimeID: expectedName})
	}

	entries, err := os.ReadDir(r.cfg.VolumesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("discover docker volumes: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path, err := safeRuntimeChildPath(r.cfg.VolumesDir, "volume ID", entry.Name())
		if err != nil {
			return nil, err
		}
		resources = append(resources, agent.RuntimeResource{VolumeID: entry.Name(), RuntimeID: path})
	}
	return resources, nil
}

func (r *DockerRuntime) ReconcileWithCleanup(ctx context.Context, state *agentv1.DesiredNodeState, allowCleanup bool) (*agentv1.StatusReport, error) {
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
		if err := r.pruneStaleEnvironmentNetworks(ctx, desiredServices); err != nil {
			return nil, err
		}
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
			AllocationIpv4:           svc.GetPrivateIpv4(),
			AllocationIpv6:           svc.GetPrivateIpv6(),
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
			cond.AllocationIpv4 = status.AllocationIPv4
			cond.AllocationIpv6 = status.AllocationIPv6
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
		cond.AllocationIpv4 = status.AllocationIPv4
		cond.AllocationIpv6 = status.AllocationIPv6
		check := svc.GetSpec().GetRuntime().GetHealthCheck()
		if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
			cond.Healthy = true
			cond.HealthyIpv4Ports = healthyFamilyPorts(status.AllocationIPv4 != "", runtimeutil.ReadinessPorts(svc))
			cond.HealthyIpv6Ports = healthyFamilyPorts(status.AllocationIPv6 != "", runtimeutil.ReadinessPorts(svc))
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
			cond.HealthyIpv4Ports = append([]int32(nil), ready.healthyIPv4Ports...)
			cond.HealthyIpv6Ports = append([]int32(nil), ready.healthyIPv6Ports...)
			cond.Phase = "Healthy"
			cond.Message = "HTTP readiness check passed"
			report.Services = append(report.Services, cond)
			continue
		}
		probe := probeDockerReadiness(status.inspect, svc)
		if probe.healthy {
			ports := runtimeutil.ReadinessPorts(svc)
			if r.ready == nil {
				r.ready = make(map[string]dockerRolloutReadiness)
			}
			r.ready[svc.GetAllocationId()] = dockerRolloutReadiness{
				rolloutGeneration: svc.GetDesiredRolloutGeneration(),
				healthyIPv4Ports:  healthyFamilyPorts(status.AllocationIPv4 != "", ports),
				healthyIPv6Ports:  healthyFamilyPorts(status.AllocationIPv6 != "", ports),
			}
			cond.Healthy = true
			cond.HealthyIpv4Ports = healthyFamilyPorts(status.AllocationIPv4 != "", ports)
			cond.HealthyIpv6Ports = healthyFamilyPorts(status.AllocationIPv6 != "", ports)
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
	AllocationIPv4           string
	AllocationIPv6           string
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
			return dockerServiceStatusFor(inspect, svc), false, nil
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
	return dockerServiceStatusFor(inspect, svc), true, nil
}

func dockerServiceStatusFor(inspect dockerContainerInspect, svc *agentv1.DesiredService) dockerServiceStatus {
	return dockerServiceStatus{
		AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
		AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		AllocationIPv4:           svc.GetPrivateIpv4(),
		AllocationIPv6:           svc.GetPrivateIpv6(),
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
		"--label", meshlabels.DefaultEnvironmentKey + "=" + strconv.FormatUint(uint64(svc.GetNetworkIdentity()), 10),
		"--label", meshlabels.DefaultIPv4Key + "=" + svc.GetPrivateIpv4(),
		"--label", meshlabels.DefaultIPv6Key + "=" + svc.GetPrivateIpv6(),
		"--label", internalHostnameLabel + "=" + svc.GetInternalHostname(),
	}
	args = append(args, dockerSandboxArgs()...)
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

func dockerSandboxArgs() []string {
	args := []string{
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "DAC_OVERRIDE",
		"--cap-add", "FOWNER",
		"--cap-add", "FSETID",
		"--cap-add", "SETGID",
		"--cap-add", "SETUID",
		"--cap-add", "SETPCAP",
		"--cap-add", "NET_BIND_SERVICE",
		"--cap-add", "KILL",
		"--pids-limit", "256",
		"--oom-score-adj", "500",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
		"--tmpfs", "/var/tmp:rw,noexec,nosuid,nodev,size=64m",
		"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=16m",
	}
	return args
}
