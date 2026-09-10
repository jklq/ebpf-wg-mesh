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

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/meshlabels"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	cnetns "github.com/containerd/containerd/pkg/netns"
	cni "github.com/containerd/go-cni"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func (e *containerdEngine) specOpts(svc *agentv1.DesiredService, image containerd.Image, netnsPath, hostsPath string) ([]oci.SpecOpts, error) {
	runtime := svc.GetSpec().GetRuntime()
	allowedBindMounts := map[string]sandboxBindMount{
		"/etc/hosts":       {source: hostsPath},
		"/etc/resolv.conf": {source: "/etc/resolv.conf"},
	}
	var writableVolumePath string
	opts := []oci.SpecOpts{
		oci.WithDefaultSpec(),
		oci.WithDefaultPathEnv,
		oci.WithDefaultUnixDevices,
		oci.WithNoNewPrivileges,
		withServiceHostsFile(hostsPath),
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
		volumePath, err := runtimeChildPath(e.cfg.Runtime.VolumesDir, "volume ID", svc.GetVolumeId())
		if err != nil {
			return nil, err
		}
		opts = append(opts, oci.WithMounts([]specs.Mount{{
			Source:      volumePath,
			Destination: defaultVolumeMount,
			Type:        "bind",
			Options:     []string{"rbind", "rw"},
		}}))
		allowedBindMounts[defaultVolumeMount] = sandboxBindMount{source: volumePath, writable: true}
		writableVolumePath = volumePath
		opts = append(opts, oci.WithEnv([]string{"PLATFORM_VOLUME_DIR=" + defaultVolumeMount}))
	}
	if isManagedDashboardService(svc) {
		secretsDir := filepath.Clean(e.cfg.Runtime.ManagedDashboardSecretsDir)
		if secretsDir == "." || !filepath.IsAbs(secretsDir) {
			return nil, errors.New("managed dashboard secrets directory is not configured on this agent")
		}
		opts = append(opts, oci.WithMounts([]specs.Mount{{
			Source:      secretsDir,
			Destination: managedDashboardSecretMount,
			Type:        "bind",
			Options:     []string{"rbind", "ro"},
		}}))
		allowedBindMounts[managedDashboardSecretMount] = sandboxBindMount{source: secretsDir}
	}
	if !e.cfg.Runtime.DisableCgroups {
		memoryMebibytes := effectiveMemoryMebibytes(runtime)
		opts = append(opts, oci.WithMemoryLimit(uint64(memoryMebibytes)*1024*1024))
		opts = append(opts, withHardMemoryIsolation())
		quota, period := cpuCFSForMillis(effectiveCPUMillis(runtime))
		opts = append(opts, oci.WithCPUCFS(quota, period))
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
	cgroupPath := ""
	if e.workloadCgroupParent != "" {
		cgroupPath = filepath.Join(e.workloadCgroupParent, containerName(svc.GetAllocationId()))
	}
	opts = append(opts, workloadSandboxOpts(allowedBindMounts, cgroupPath)...)
	if writableVolumePath != "" {
		opts = append(opts, withWritableVolumeOwnership(writableVolumePath))
	}
	if e.cfg.Runtime.DisableCgroups {
		opts = append(opts, withoutCgroups)
	}
	return opts, nil
}

func withWritableVolumeOwnership(path string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Process == nil {
			return errors.New("writable volume ownership requires a process identity")
		}
		if err := os.Chown(path, int(spec.Process.User.UID), int(spec.Process.User.GID)); err != nil {
			return fmt.Errorf("set writable volume ownership: %w", err)
		}
		return nil
	}
}

func withHardMemoryIsolation() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		if spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil {
			return errors.New("hard memory isolation requires a memory limit")
		}
		memory := spec.Linux.Resources.Memory
		memory.Swap = memory.Limit
		disableOOMKiller := false
		memory.DisableOOMKiller = &disableOOMKiller
		if spec.Linux.Resources.Unified == nil {
			spec.Linux.Resources.Unified = make(map[string]string)
		}
		spec.Linux.Resources.Unified["memory.oom.group"] = "1"
		return nil
	}
}

func withServiceHostsFile(path string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *specs.Spec) error {
		spec.Mounts = append(spec.Mounts, specs.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      path,
			Options:     []string{"rbind", "ro"},
		})
		return nil
	}
}

func (e *containerdEngine) ensureServiceHostsFile(svc *agentv1.DesiredService) (string, error) {
	path, err := runtimeChildPath(filepath.Join(e.cfg.Runtime.DataDir, serviceHostsDir), "allocation ID", svc.GetAllocationId())
	if err != nil {
		return "", err
	}
	contents, err := renderServiceHostsFile(svc.GetInternalHosts())
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return "", fmt.Errorf("write service hosts file: %w", err)
	}
	return path, nil
}

func (e *containerdEngine) cleanupServiceHostsFile(allocationID string) error {
	path, err := runtimeChildPath(filepath.Join(e.cfg.Runtime.DataDir, serviceHostsDir), "allocation ID", allocationID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove service hosts file: %w", err)
	}
	return nil
}

func effectiveCPUMillis(runtime *platformv1.ServiceRuntime) int64 {
	if runtime.GetCpuMillis() > 0 {
		return runtime.GetCpuMillis()
	}
	return defaultServiceCPUMillis
}

func effectiveMemoryMebibytes(runtime *platformv1.ServiceRuntime) int64 {
	if runtime.GetMemoryMebibytes() > 0 {
		return runtime.GetMemoryMebibytes()
	}
	return defaultServiceMemoryMebibytes
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
	ipv4, _ := netip.ParseAddr(svc.GetPrivateIpv4())
	ipv6, _ := netip.ParseAddr(svc.GetPrivateIpv6())
	maps.Copy(labels, e.cfg.Containerd.LabelKeys().Encode(meshlabels.Identity{
		NetworkIdentity: svc.GetNetworkIdentity(),
		IPv4:            ipv4,
		IPv6:            ipv6,
	}))
	return labels
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
			"K8S_POD_NAMESPACE": svc.GetEnvironmentId(),
		}),
	}
	requestedIPs := make([]string, 0, 2)
	if svc.GetPrivateIpv4() != "" {
		requestedIPs = append(requestedIPs, svc.GetPrivateIpv4())
	}
	if svc.GetPrivateIpv6() != "" {
		requestedIPs = append(requestedIPs, svc.GetPrivateIpv6())
	}
	if len(requestedIPs) != 0 {
		opts = append(opts, cni.WithCapability("ips", requestedIPs))
	}
	result, err := e.cni.Setup(ctx, containerID, netnsPath, opts...)
	if err != nil {
		return nil, fmt.Errorf("cni setup %s: %w", containerID, err)
	}
	for _, requestedIP := range requestedIPs {
		if !resultHasIP(result, requestedIP) {
			_ = e.cni.Remove(ctx, containerID, netnsPath, opts...)
			return nil, fmt.Errorf("cni did not assign requested ip %s", requestedIP)
		}
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
