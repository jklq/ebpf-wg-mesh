//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	cnetns "github.com/containerd/containerd/pkg/netns"
	cni "github.com/containerd/go-cni"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	defaultCNIPluginDir = "/usr/lib/cni"
	defaultCNIConfDir   = "/etc/cni/net.d"
	defaultNetworkName  = "mesh-cni"
)

type containerdEngine struct {
	cfg         config.AgentConfig
	client      *containerd.Client
	cni         cni.CNI
	logSinkMu   sync.RWMutex
	logSink     LogSink
	logSequence atomic.Uint64
}

func newContainerdEngine(cfg config.AgentConfig) (serviceEngine, error) {
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
	return &containerdEngine{cfg: cfg, client: client, cni: netPlugin}, nil
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

func (e *containerdEngine) EnsureService(ctx context.Context, svc *agentv1.DesiredService) (serviceStatus, bool, error) {
	if svc.GetNetworkIdentity() == 0 {
		return serviceStatus{}, false, fmt.Errorf("service %s missing network identity", svc.GetServiceId())
	}
	ctx = e.namespaced(ctx)
	containerID := containerName(svc.GetAllocationId())
	if rec, exists, err := e.inspect(ctx, containerID); err != nil {
		return serviceStatus{}, false, err
	} else if exists {
		if rec.rolloutGeneration == svc.GetDesiredRolloutGeneration() &&
			rec.networkIdentity == svc.GetNetworkIdentity() && rec.running {
			return serviceStatus{
				AppliedSpecRevision:      svc.GetDesiredSpecRevision(),
				AppliedRolloutGeneration: rec.rolloutGeneration,
				AllocationIP:             allocationIPForService(svc),
			}, false, nil
		}
		if err := e.RemoveService(ctx, svc.GetAllocationId()); err != nil {
			return serviceStatus{}, false, err
		}
	}

	image, err := e.ensureImage(ctx, svc.GetSpec().GetImage())
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

	container, err := e.client.NewContainer(ctx, containerID,
		containerd.WithSnapshotter(e.cfg.Runtime.Snapshotter),
		containerd.WithNewSnapshot(containerID, image),
		containerd.WithNewSpec(e.specOpts(svc, image, netnsPath)...),
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
		AllocationIP:             allocationIPForService(svc),
	}, true, nil
}

func (e *containerdEngine) logIOCreator(svc *agentv1.DesiredService) cio.Creator {
	stdout := &containerLogWriter{
		projectID:         svc.GetProjectId(),
		serviceID:         svc.GetServiceId(),
		allocationID:      svc.GetAllocationId(),
		stream:            "stdout",
		rolloutGeneration: svc.GetDesiredRolloutGeneration(),
		nextSequence:      e.nextLogSequence,
		sink:              e.currentLogSink,
	}
	stderr := &containerLogWriter{
		projectID:         svc.GetProjectId(),
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
	return errors.Join(errs...)
}

func (e *containerdEngine) deleteTask(ctx context.Context, task containerd.Task, containerID string) error {
	exitCh, err := task.Wait(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
		if !strings.Contains(err.Error(), "not found") {
			return err
		}
	}
	if exitCh != nil {
		select {
		case <-exitCh:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("wait for task %s exit: timeout", containerID)
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
	networkIdentity := e.cfg.Containerd.LabelKeys().ProjectID(info.Labels)
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
	return inspectRecord{rolloutGeneration: rolloutGeneration, networkIdentity: networkIdentity, running: status.Status == containerd.Running}, true, nil
}

func (e *containerdEngine) ensureImage(ctx context.Context, ref string) (containerd.Image, error) {
	image, err := e.client.GetImage(ctx, ref)
	if err == nil {
		return image, nil
	}
	if !errdefs.IsNotFound(err) {
		return nil, err
	}
	return e.client.Pull(ctx, ref, containerd.WithPullUnpack, containerd.WithPullSnapshotter(e.cfg.Runtime.Snapshotter))
}

func (e *containerdEngine) specOpts(svc *agentv1.DesiredService, image containerd.Image, netnsPath string) []oci.SpecOpts {
	runtime := svc.GetSpec().GetRuntime()
	opts := []oci.SpecOpts{
		oci.WithDefaultSpec(),
		oci.WithDefaultPathEnv,
		oci.WithDefaultUnixDevices,
		oci.WithNoNewPrivileges,
		oci.WithCapabilities(nil),
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
		oci.WithHostname(svc.GetName()),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: netnsPath}),
	}
	if len(runtime.GetEnv()) > 0 {
		envs := make([]string, 0, len(runtime.GetEnv()))
		keys := make([]string, 0, len(runtime.GetEnv()))
		for key := range runtime.GetEnv() {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var b strings.Builder
			b.Grow(len(key) + 1 + len(runtime.GetEnv()[key]))
			b.WriteString(key)
			b.WriteByte('=')
			b.WriteString(runtime.GetEnv()[key])
			envs = append(envs, b.String())
		}
		opts = append(opts, oci.WithEnv(envs))
	}
	if svc.GetVolumeId() != "" {
		opts = append(opts, oci.WithMounts([]specs.Mount{{
			Source:      filepath.Join(e.cfg.Runtime.VolumesDir, svc.GetVolumeId()),
			Destination: defaultVolumeMount,
			Type:        "bind",
			Options:     []string{"rbind", "rw"},
		}}))
		opts = append(opts, oci.WithEnv([]string{"PLATFORM_VOLUME_DIR=" + defaultVolumeMount}))
	}
	if !e.cfg.Runtime.DisableCgroups && runtime.GetMemoryMebibytes() > 0 {
		opts = append(opts, oci.WithMemoryLimit(uint64(runtime.GetMemoryMebibytes())*1024*1024))
	}
	if !e.cfg.Runtime.DisableCgroups && runtime.GetCpuMillis() > 0 {
		opts = append(opts, oci.WithCPUs(fmt.Sprintf("%.3f", float64(runtime.GetCpuMillis())/1000.0)))
	}
	if cmd := runtime.GetCommand(); len(cmd) > 0 {
		args := append([]string{}, cmd...)
		args = append(args, runtime.GetArgs()...)
		opts = append(opts, oci.WithImageConfig(image), oci.WithProcessArgs(args...))
	} else if len(runtime.GetArgs()) > 0 {
		opts = append(opts, oci.WithImageConfigArgs(image, runtime.GetArgs()))
	} else {
		opts = append(opts, oci.WithImageConfig(image))
	}
	if e.cfg.Runtime.DisableCgroups {
		opts = append(opts, withoutCgroups)
	}
	return opts
}

func withoutCgroups(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
	if s.Linux == nil {
		s.Linux = &specs.Linux{}
	}
	s.Linux.CgroupsPath = ""
	if s.Linux.Resources == nil {
		return nil
	}
	s.Linux.Resources.Memory = nil
	s.Linux.Resources.CPU = nil
	s.Linux.Resources.Pids = nil
	s.Linux.Resources.BlockIO = nil
	s.Linux.Resources.HugepageLimits = nil
	s.Linux.Resources.Network = nil
	s.Linux.Resources.Rdma = nil
	return nil
}

func (e *containerdEngine) serviceLabels(svc *agentv1.DesiredService) map[string]string {
	labels := map[string]string{
		meshlabels.Managed:                  "true",
		meshlabels.AllocationID:             svc.GetAllocationId(),
		meshlabels.ServiceID:                svc.GetServiceId(),
		meshlabels.DesiredSpecRevision:      strconv.FormatInt(svc.GetDesiredSpecRevision(), 10),
		meshlabels.DesiredRolloutGeneration: strconv.FormatInt(svc.GetDesiredRolloutGeneration(), 10),
	}
	// The firewall recovers this identity from the labels once the task starts.
	ipv6, _ := netip.ParseAddr(svc.GetPrivateIpv6())
	maps.Copy(labels, e.cfg.Containerd.LabelKeys().Encode(meshlabels.Identity{
		ProjectID: svc.GetNetworkIdentity(),
		IPv6:      ipv6,
	}))
	return labels
}

func allocationIPForService(svc *agentv1.DesiredService) string {
	return svc.GetPrivateIpv6()
}

func (e *containerdEngine) namespaced(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, e.cfg.Containerd.Namespace)
}

func (e *containerdEngine) createNetNS(allocationID string) (string, bool, error) {
	if path, ok := e.netnsPath(allocationID); ok {
		return path, false, nil
	}
	ns, err := cnetns.NewNetNS(filepath.Join(e.cfg.Runtime.DataDir, "netns"))
	if err != nil {
		return "", false, fmt.Errorf("create netns %s: %w", allocationID, err)
	}
	return ns.GetPath(), true, nil
}

func (e *containerdEngine) setupNetwork(ctx context.Context, containerID, netnsPath string, svc *agentv1.DesiredService) (*cni.Result, error) {
	opts := []cni.NamespaceOpts{
		cni.WithArgs("IgnoreUnknown", "1"),
		cni.WithLabels(map[string]string{
			"K8S_POD_NAME":      svc.GetName(),
			"K8S_POD_NAMESPACE": svc.GetProjectId(),
		}),
	}
	if svc.GetPrivateIpv6() != "" {
		opts = append(opts, cni.WithArgs("IP", svc.GetPrivateIpv6()))
		opts = append(opts, cni.WithCapability("ips", []string{svc.GetPrivateIpv6()}))
	}
	result, err := e.cni.Setup(ctx, containerID, netnsPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("cni setup %s: %w", containerID, err)
	}
	if svc.GetPrivateIpv6() != "" && !resultHasIP(result, svc.GetPrivateIpv6()) {
		_ = e.cni.Remove(ctx, containerID, netnsPath, opts...)
		return nil, fmt.Errorf("cni did not assign requested ip %s", svc.GetPrivateIpv6())
	}
	return result, nil
}

func (e *containerdEngine) teardownNetwork(ctx context.Context, containerID, allocationID, netnsPath string) error {
	if netnsPath == "" {
		return nil
	}
	err := e.cni.Remove(ctx, containerID, netnsPath,
		cni.WithArgs("IgnoreUnknown", "1"),
	)
	if err != nil && !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
		return fmt.Errorf("cni remove %s: %w", allocationID, err)
	}
	return nil
}

func resultHasIP(result *cni.Result, want string) bool {
	if result == nil {
		return false
	}
	for _, cfg := range result.Interfaces {
		for _, ipCfg := range cfg.IPConfigs {
			if ipCfg.IP.String() == want {
				return true
			}
		}
	}
	return false
}

func (e *containerdEngine) persistNetNSPath(allocationID, netnsPath string) error {
	return os.WriteFile(e.netnsFile(allocationID), []byte(netnsPath), 0o644)
}

func (e *containerdEngine) cleanupNetNS(allocationID string) error {
	path, ok := e.netnsPath(allocationID)
	if ok {
		_ = cnetns.LoadNetNS(path).Remove()
	}
	err := os.Remove(e.netnsFile(allocationID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (e *containerdEngine) netnsPath(allocationID string) (string, bool) {
	b, err := os.ReadFile(e.netnsFile(allocationID))
	if err != nil {
		return "", false
	}
	path := string(b)
	if path == "" {
		return "", false
	}
	return path, true
}

func (e *containerdEngine) netnsFile(allocationID string) string {
	return filepath.Join(e.cfg.Runtime.DataDir, "netns", allocationID+".path")
}
