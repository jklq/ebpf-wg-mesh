package localteststack

import (
	"context"
	"crypto/sha256"
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
	"ebof-wg-mesh/internal/runtimeutil"
)

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

func (r *DockerRuntime) environmentNetworkPrefix() string {
	return r.cfg.DockerNetwork + "-env-"
}

func (r *DockerRuntime) pruneStaleEnvironmentNetworks(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	keep := make(map[string]struct{})
	for _, svc := range desired {
		if name := r.environmentNetworkName(svc.GetEnvironmentId()); name != r.cfg.DockerNetwork {
			keep[name] = struct{}{}
		}
	}
	names, err := listDockerNetworks(ctx, r.runner)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, r.environmentNetworkPrefix()) {
			continue
		}
		if _, ok := keep[name]; ok {
			continue
		}
		if _, err := r.runner.Run(ctx, "network", "rm", name); err != nil {
			if isDockerMissingObjectError(err) || isDockerActiveEndpointsError(err) {
				continue
			}
			return fmt.Errorf("remove stale docker network %s: %w", name, err)
		}
	}
	return nil
}

func (r *DockerRuntime) pruneStaleServices(ctx context.Context, desired map[string]*agentv1.DesiredService) error {
	staleCandidates := make(map[string]struct{})
	resources, err := r.DiscoverRuntimeResources(ctx)
	if err != nil {
		return err
	}
	for _, resource := range resources {
		if resource.AllocationID != "" {
			staleCandidates[resource.AllocationID] = struct{}{}
		}
	}

	paths, err := filepath.Glob(filepath.Join(r.cfg.DataDir, "desired", "*.json"))
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
		if err := r.removeService(ctx, allocationID); err != nil {
			return err
		}
		delete(r.ready, allocationID)
		path, err := safeRuntimeChildPath(filepath.Join(r.cfg.DataDir, "desired"), "allocation ID", allocationID)
		if err != nil {
			return err
		}
		if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove desired file %s: %w", path+".json", err)
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
	marker := fmt.Sprintf("{\"allocation_id\":%q}\n", svc.GetAllocationId())
	if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
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

func healthyFamilyPorts(healthy bool, ports []int32) []int32 {
	if !healthy {
		return nil
	}
	return append([]int32(nil), ports...)
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
	port := runtimeutil.ReadinessCheckPort(runtime, check)
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
		labels[meshlabels.DefaultEnvironmentKey] == strconv.FormatUint(uint64(svc.GetNetworkIdentity()), 10) &&
		labels[meshlabels.DefaultIPv4Key] == svc.GetPrivateIpv4() &&
		labels[meshlabels.DefaultIPv6Key] == svc.GetPrivateIpv6() &&
		labels[internalHostnameLabel] == svc.GetInternalHostname()
}

func maxInt32(v int32, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}
