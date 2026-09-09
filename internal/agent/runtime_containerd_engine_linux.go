//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/meshlabels"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/remotes/docker"
	cni "github.com/containerd/go-cni"
)

const (
	defaultCNIPluginDir                 = "/usr/lib/cni"
	defaultCNIConfDir                   = "/etc/cni/net.d"
	defaultNetworkName                  = "mesh-cni"
	serviceHostsDir                     = "hosts"
	defaultServiceCPUMillis       int64 = 250
	defaultServiceMemoryMebibytes int64 = 256
)

type containerdEngine struct {
	cfg                  config.AgentConfig
	client               *containerd.Client
	cni                  cni.CNI
	logSinkMu            sync.RWMutex
	logSink              LogSink
	logSequence          atomic.Uint64
	oomMu                sync.Mutex
	oom                  map[string]bool
	workloadCgroupParent string
}

func newContainerdEngine(cfg config.AgentConfig) (serviceEngine, error) {
	workloadCgroupParent, err := prepareWorkloadSandboxHost(cfg)
	if err != nil {
		return nil, err
	}
	client, err := containerd.New(
		cfg.Containerd.Socket,
		containerd.WithDefaultNamespace(cfg.Containerd.Namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("connect containerd: %w", err)
	}
	netPlugin, err := cni.New(
		cni.WithPluginDir([]string{defaultCNIPluginDir}),
		cni.WithPluginConfDir(defaultCNIConfDir),
		cni.WithInterfacePrefix("eth"),
	)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("create cni client: %w", err)
	}
	if err := netPlugin.Load(cni.WithLoNetwork, cni.WithDefaultConf); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("load cni config: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.DataDir, "netns"), 0o755); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mkdir netns dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Runtime.DataDir, serviceHostsDir), 0o755); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mkdir hosts dir: %w", err)
	}
	return &containerdEngine{cfg: cfg, client: client, cni: netPlugin, workloadCgroupParent: workloadCgroupParent}, nil
}

func (e *containerdEngine) Close() error {
	if e == nil || e.client == nil {
		return nil
	}
	return e.client.Close()
}

func (e *containerdEngine) SetLogSink(sink LogSink) {
	if e == nil {
		return
	}
	e.logSinkMu.Lock()
	defer e.logSinkMu.Unlock()
	e.logSink = sink
}

func (e *containerdEngine) DiscoverServices(ctx context.Context) ([]RuntimeResource, error) {
	containers, err := e.client.Containers(e.namespaced(ctx))
	if err != nil {
		return nil, fmt.Errorf("list containerd resources: %w", err)
	}
	resources := make([]RuntimeResource, 0, len(containers))
	for _, container := range containers {
		info, err := container.Info(e.namespaced(ctx))
		if err != nil {
			return nil, fmt.Errorf("inspect containerd resource %s: %w", container.ID(), err)
		}
		if info.Labels[meshlabels.Managed] != "true" {
			continue
		}
		allocationID := strings.TrimSpace(info.Labels[meshlabels.AllocationID])
		if err := validateRuntimeID("allocation ID", allocationID); err != nil {
			return nil, fmt.Errorf("managed runtime resource %s: %w", container.ID(), err)
		}
		if container.ID() != containerName(allocationID) {
			return nil, fmt.Errorf("managed runtime resource %s does not match stable allocation identity %s", container.ID(), allocationID)
		}
		resources = append(resources, RuntimeResource{AllocationID: allocationID, RuntimeID: container.ID()})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].AllocationID < resources[j].AllocationID })
	return resources, nil
}

func (e *containerdEngine) ReconcileEvents(ctx context.Context) (<-chan struct{}, <-chan error) {
	events, subscriptionErrors := e.client.Subscribe(
		e.namespaced(ctx),
		`topic=="/tasks/exit",event.container_id~="^platform-"`,
		`topic=="/tasks/oom",event.container_id~="^platform-"`,
	)
	out := make(chan struct{}, 1)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if event == nil {
					continue
				}
				if strings.Contains(event.Topic, "/tasks/oom") {
					e.noteOOMTopic(event.Topic, event.Event)
				}
				select {
				case out <- struct{}{}:
				default:
				}
			case err, ok := <-subscriptionErrors:
				if !ok || err == nil {
					return
				}
				select {
				case errs <- err:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out, errs
}

func (e *containerdEngine) EnsureService(ctx context.Context, svc *agentv1.DesiredService) (serviceStatus, bool, error) {
	if svc.GetNetworkIdentity() == 0 {
		return serviceStatus{}, false, fmt.Errorf("service %s missing network identity", svc.GetServiceId())
	}
	if ipv4, err := netip.ParseAddr(svc.GetPrivateIpv4()); err != nil || !ipv4.Is4() {
		return serviceStatus{}, false, fmt.Errorf("service %s has invalid private IPv4 %q", svc.GetServiceId(), svc.GetPrivateIpv4())
	}
	if ipv6, err := netip.ParseAddr(svc.GetPrivateIpv6()); err != nil || !ipv6.Is6() {
		return serviceStatus{}, false, fmt.Errorf("service %s has invalid private IPv6 %q", svc.GetServiceId(), svc.GetPrivateIpv6())
	}
	if err := validateRuntimeID("allocation ID", svc.GetAllocationId()); err != nil {
		return serviceStatus{}, false, err
	}
	if volumeID := svc.GetVolumeId(); volumeID != "" {
		if _, err := runtimeChildPath(e.cfg.Runtime.VolumesDir, "volume ID", volumeID); err != nil {
			return serviceStatus{}, false, err
		}
	}
	if isManagedDashboardService(svc) {
		secretsDir := filepath.Clean(e.cfg.Runtime.ManagedDashboardSecretsDir)
		if secretsDir == "." || !filepath.IsAbs(secretsDir) {
			return serviceStatus{}, false, errors.New("managed dashboard secrets directory is not configured on this agent")
		}
	}
	hostsPath, err := e.ensureServiceHostsFile(svc)
	if err != nil {
		return serviceStatus{}, false, err
	}
	ctx = e.namespaced(ctx)
	containerID := containerName(svc.GetAllocationId())
	if rec, exists, err := e.inspect(ctx, containerID); err != nil {
		return serviceStatus{}, false, err
	} else if exists {
		if rec.rolloutGeneration == svc.GetDesiredRolloutGeneration() &&
			rec.networkIdentity == svc.GetNetworkIdentity() {
			netnsPath, _ := e.netnsPath(svc.GetAllocationId())
			return serviceStatus{
				AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
				AppliedRolloutGeneration: rec.rolloutGeneration,
				AllocationIPv4:           svc.GetPrivateIpv4(),
				AllocationIPv6:           svc.GetPrivateIpv6(),
				NetworkNamespacePath:     netnsPath,
				Running:                  rec.running,
				ExitCode:                 rec.exitCode,
				Signal:                   rec.signal,
				OOMKilled:                rec.oomKilled,
			}, false, nil
		}
		if err := e.RemoveService(ctx, svc.GetAllocationId()); err != nil {
			return serviceStatus{}, false, err
		}
		hostsPath, err = e.ensureServiceHostsFile(svc)
		if err != nil {
			return serviceStatus{}, false, err
		}
	}

	image, err := e.ensureImage(ctx, svc.GetSpec().GetImage(), svc.GetRegistryUsername(), svc.GetRegistryPassword())
	if err != nil {
		return serviceStatus{}, false, err
	}
	netnsPath, createdNS, err := e.createNetNS(svc.GetAllocationId())
	if err != nil {
		return serviceStatus{}, false, err
	}
	if createdNS {
		if err := e.persistNetNSPath(svc.GetAllocationId(), netnsPath); err != nil {
			return serviceStatus{}, false, err
		}
	}

	specOpts, err := e.specOpts(svc, image, netnsPath, hostsPath)
	if err != nil {
		_ = e.cleanupNetNS(svc.GetAllocationId())
		return serviceStatus{}, false, err
	}
	container, err := e.client.NewContainer(ctx, containerID,
		containerd.WithSnapshotter(e.cfg.Runtime.Snapshotter),
		containerd.WithNewSnapshot(containerID, image),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(e.serviceLabels(svc)),
	)
	if err != nil {
		_ = e.cleanupNetNS(svc.GetAllocationId())
		return serviceStatus{}, false, fmt.Errorf("create container %s: %w", containerID, err)
	}
	cleanup := func(err error) (serviceStatus, bool, error) {
		_ = e.teardownNetwork(ctx, containerID, svc.GetAllocationId(), netnsPath)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		_ = e.cleanupNetNS(svc.GetAllocationId())
		return serviceStatus{}, false, err
	}

	if _, err := e.setupNetwork(ctx, containerID, netnsPath, svc); err != nil {
		return cleanup(err)
	}
	task, err := container.NewTask(ctx, e.logIOCreator(svc))
	if err != nil {
		return cleanup(fmt.Errorf("create task %s: %w", containerID, err))
	}
	if err := task.Start(ctx); err != nil {
		_ = e.deleteTask(ctx, task, containerID)
		return cleanup(fmt.Errorf("start task %s: %w", containerID, err))
	}
	return serviceStatus{
		AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
		AppliedRolloutGeneration: svc.GetDesiredRolloutGeneration(),
		AllocationIPv4:           svc.GetPrivateIpv4(),
		AllocationIPv6:           svc.GetPrivateIpv6(),
		NetworkNamespacePath:     netnsPath,
		Running:                  true,
	}, true, nil
}

func (e *containerdEngine) logIOCreator(svc *agentv1.DesiredService) cio.Creator {
	stdout := &containerLogWriter{
		environmentID:     svc.GetEnvironmentId(),
		serviceID:         svc.GetServiceId(),
		allocationID:      svc.GetAllocationId(),
		stream:            "stdout",
		rolloutGeneration: svc.GetDesiredRolloutGeneration(),
		nextSequence:      e.nextLogSequence,
		sink:              e.currentLogSink,
	}
	stderr := &containerLogWriter{
		environmentID:     svc.GetEnvironmentId(),
		serviceID:         svc.GetServiceId(),
		allocationID:      svc.GetAllocationId(),
		stream:            "stderr",
		rolloutGeneration: svc.GetDesiredRolloutGeneration(),
		nextSequence:      e.nextLogSequence,
		sink:              e.currentLogSink,
	}
	return cio.NewCreator(cio.WithStreams(nil, stdout, stderr))
}

func (e *containerdEngine) nextLogSequence() uint64 {
	return e.logSequence.Add(1)
}

func (e *containerdEngine) currentLogSink() LogSink {
	if e == nil {
		return nil
	}
	e.logSinkMu.RLock()
	defer e.logSinkMu.RUnlock()
	return e.logSink
}

func (e *containerdEngine) RemoveService(ctx context.Context, allocationID string) error {
	ctx = e.namespaced(ctx)
	containerID := containerName(allocationID)
	e.clearOOM(containerID)
	var errs []error
	container, err := e.client.LoadContainer(ctx, containerID)
	if err == nil {
		if task, err := container.Task(ctx, nil); err == nil {
			if err := e.deleteTask(ctx, task, containerID); err != nil && !errdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete task %s: %w", containerID, err))
			}
		} else if !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("load task %s: %w", containerID, err))
		}
		if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete container %s: %w", containerID, err))
		}
	} else if !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("load container %s: %w", containerID, err))
	}

	if netnsPath, ok := e.netnsPath(allocationID); ok {
		if err := e.teardownNetwork(ctx, containerID, allocationID, netnsPath); err != nil {
			errs = append(errs, err)
		}
		if err := e.cleanupNetNS(allocationID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := e.cleanupServiceHostsFile(allocationID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DrainService asks the workload to exit with SIGTERM and keeps its container
// and network namespace intact until it exits or the absolute deadline passes.
// SIGKILL is never used before that deadline.
func (e *containerdEngine) DrainService(ctx context.Context, allocationID string, deadline time.Time) (bool, bool, error) {
	ctx = e.namespaced(ctx)
	containerID := containerName(allocationID)
	container, err := e.client.LoadContainer(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return true, false, nil
		}
		return false, false, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			if err := e.cleanupStoppedService(ctx, container, containerID, allocationID); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
		return false, false, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return false, false, err
	}
	if status.Status == containerd.Running && time.Now().UTC().Before(deadline.UTC()) {
		if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
			return false, false, fmt.Errorf("signal task %s with SIGTERM: %w", containerID, err)
		}
		return false, false, nil
	}
	forced := status.Status == containerd.Running
	if forced {
		exitCh, waitErr := task.Wait(ctx)
		if waitErr != nil && !errdefs.IsNotFound(waitErr) {
			return false, false, fmt.Errorf("wait for draining task %s: %w", containerID, waitErr)
		}
		if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			return false, false, fmt.Errorf("signal task %s with SIGKILL: %w", containerID, err)
		}
		if exitCh != nil {
			select {
			case <-exitCh:
			case <-ctx.Done():
				return false, false, ctx.Err()
			case <-time.After(10 * time.Second):
				return false, false, fmt.Errorf("wait for force-killed task %s: timeout", containerID)
			}
		}
	}
	if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return false, forced, fmt.Errorf("delete drained task %s: %w", containerID, err)
	}
	if err := e.cleanupStoppedService(ctx, container, containerID, allocationID); err != nil {
		return false, forced, err
	}
	return true, forced, nil
}

func (e *containerdEngine) cleanupStoppedService(ctx context.Context, container containerd.Container, containerID, allocationID string) error {
	var errs []error
	if netnsPath, ok := e.netnsPath(allocationID); ok {
		if err := e.teardownNetwork(ctx, containerID, allocationID, netnsPath); err != nil {
			errs = append(errs, err)
		}
		if err := e.cleanupNetNS(allocationID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, err)
	}
	if err := e.cleanupServiceHostsFile(allocationID); err != nil {
		errs = append(errs, err)
	}
	e.clearOOM(containerID)
	return errors.Join(errs...)
}

func (e *containerdEngine) deleteTask(ctx context.Context, task containerd.Task, containerID string) error {
	exitCh, err := task.Wait(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
		if !strings.Contains(err.Error(), "not found") {
			return err
		}
	}
	if exitCh != nil {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-exitCh:
		case <-timer.C:
			if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) && !strings.Contains(err.Error(), "not found") {
				return err
			}
			killTimer := time.NewTimer(10 * time.Second)
			defer killTimer.Stop()
			select {
			case <-exitCh:
			case <-killTimer.C:
				return fmt.Errorf("wait for task %s after SIGKILL: timeout", containerID)
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

type inspectRecord struct {
	rolloutGeneration int64
	networkIdentity   uint32
	running           bool
	exitCode          int32
	signal            int32
	oomKilled         bool
}

func (e *containerdEngine) inspect(ctx context.Context, containerID string) (inspectRecord, bool, error) {
	container, err := e.client.LoadContainer(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return inspectRecord{}, false, nil
		}
		return inspectRecord{}, false, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return inspectRecord{}, false, err
	}
	rolloutGeneration, _ := strconv.ParseInt(info.Labels[meshlabels.DesiredRolloutGeneration], 10, 64)
	networkIdentity := e.cfg.Containerd.LabelKeys().NetworkIdentity(info.Labels)
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return inspectRecord{rolloutGeneration: rolloutGeneration, networkIdentity: networkIdentity}, true, nil
		}
		return inspectRecord{}, false, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return inspectRecord{}, false, err
	}
	rec := inspectRecord{rolloutGeneration: rolloutGeneration, networkIdentity: networkIdentity, running: status.Status == containerd.Running}
	if !rec.running {
		rec.exitCode, rec.signal = classifyContainerExit(status.ExitStatus)
		rec.oomKilled = e.containerOOMKilled(containerID)
	}
	return rec, true, nil
}

func classifyContainerExit(exitStatus uint32) (int32, int32) {
	if exitStatus > 128 && exitStatus <= 128+255 {
		return int32(exitStatus), int32(exitStatus - 128)
	}
	return int32(exitStatus), 0
}

func (e *containerdEngine) containerOOMKilled(containerID string) bool {
	if e == nil {
		return false
	}
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	return e.oom[containerID]
}

func (e *containerdEngine) clearOOM(containerID string) {
	if e == nil {
		return
	}
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	delete(e.oom, containerID)
}

func (e *containerdEngine) noteOOM(containerID string) {
	containerID = strings.TrimSpace(containerID)
	if e == nil || containerID == "" {
		return
	}
	e.oomMu.Lock()
	defer e.oomMu.Unlock()
	if e.oom == nil {
		e.oom = make(map[string]bool)
	}
	e.oom[containerID] = true
}

func (e *containerdEngine) noteOOMTopic(topic string, payload any) {
	if payload == nil {
		return
	}
	raw := fmt.Sprint(payload)
	if id := containerIDFromEventText(raw); id != "" {
		e.noteOOM(id)
		return
	}
	if id := containerIDFromEventText(topic); id != "" {
		e.noteOOM(id)
	}
}

func containerIDFromEventText(text string) string {
	const prefix = "platform-"
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return ""
	}
	id := text[idx:]
	for i, r := range id {
		if r == '"' || r == ' ' || r == ',' || r == '}' {
			id = id[:i]
			break
		}
	}
	return id
}

func (e *containerdEngine) ensureImage(ctx context.Context, ref, username, password string) (containerd.Image, error) {
	image, err := e.client.GetImage(ctx, ref)
	if err == nil {
		return image, nil
	}
	if !errdefs.IsNotFound(err) {
		return nil, err
	}
	opts := []containerd.RemoteOpt{
		containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(e.cfg.Runtime.Snapshotter),
	}
	if username != "" || password != "" {
		resolver := docker.NewResolver(docker.ResolverOptions{
			Credentials: func(string) (string, string, error) {
				return username, password, nil
			},
		})
		opts = append(opts, containerd.WithResolver(resolver))
	}
	return e.client.Pull(ctx, ref, opts...)
}
