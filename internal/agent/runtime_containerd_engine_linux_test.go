//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
)

const containerdTestImage = "docker.io/library/busybox:1.36.1"

func TestContainerdEngineCreateServeDestroy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("c")
	marker := "marker-" + allocID
	svc := busyboxHTTPService(allocID, 7, 1, "10.200.9.16", "fd00:200:0:9::10", marker)

	cleanupContainerdService(t, engine, cfg, allocID)
	if _, _, err := engine.EnsureService(ctx, svc); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService: %v", err)
	}

	assertContainerRunning(t, ctx, engine, allocID)
	netnsPath := requirePersistedNetNS(t, engine, allocID)
	if got := httpGetInNamespace(t, ctx, netnsPath, svc.GetPrivateIpv6(), 8080, "/"); got != marker {
		t.Fatalf("workload HTTP body = %q, want marker %q", got, marker)
	}

	if err := engine.RemoveService(ctx, allocID); err != nil {
		t.Fatalf("RemoveService: %v", err)
	}
	assertServiceTornDown(t, ctx, engine, cfg, allocID, netnsPath)
}

func TestContainerdEngineDrainSendsSIGTERMThenSIGKILLAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("drain")
	svc := busyboxHTTPService(allocID, 12, 1, "10.200.9.18", "fd00:200:0:9::12", "waiting")
	// Join on newlines, not "; ": a "&" already terminates the command, so
	// "httpd ... &; trap ..." is a shell syntax error and nothing ever starts.
	svc.Spec.Runtime.Args = []string{strings.Join([]string{
		"mkdir -p /tmp/www",
		"printf waiting > /tmp/www/index.html",
		"httpd -f -p [::]:8080 -h /tmp/www &",
		"trap 'printf term > /tmp/www/index.html' TERM",
		"while :; do sleep 1; done",
	}, "\n")}

	cleanupContainerdService(t, engine, cfg, allocID)
	if _, _, err := engine.EnsureService(ctx, svc); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService: %v", err)
	}
	netnsPath := requirePersistedNetNS(t, engine, allocID)
	if got := httpGetInNamespace(t, ctx, netnsPath, svc.GetPrivateIpv6(), 8080, "/"); got != "waiting" {
		t.Fatalf("initial workload marker = %q", got)
	}

	drained, forced, err := engine.DrainService(ctx, allocID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("DrainService(SIGTERM): %v", err)
	}
	if drained || forced {
		t.Fatalf("pre-deadline drain = drained %t, forced %t", drained, forced)
	}
	if got := httpGetInNamespace(t, ctx, netnsPath, svc.GetPrivateIpv6(), 8080, "/"); got != "term" {
		t.Fatalf("workload did not observe SIGTERM, marker = %q", got)
	}
	assertContainerRunning(t, ctx, engine, allocID)

	drained, forced, err = engine.DrainService(ctx, allocID, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("DrainService(SIGKILL): %v", err)
	}
	if !drained || !forced {
		t.Fatalf("expired drain = drained %t, forced %t", drained, forced)
	}
	assertServiceTornDown(t, ctx, engine, cfg, allocID, netnsPath)
}

func TestContainerdEngineGenerationReplaceAndNetnsProbeFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, cfg := newTestContainerdEngine(t)
	allocID := uniqueRuntimeID("g")
	ip := "fd00:200:0:9::11"
	first := busyboxHTTPService(allocID, 7, 3, "10.200.9.17", ip, "gen-3-"+allocID)
	first.Spec.Runtime.HealthCheck = &platformv1.HealthCheck{
		Type:           platformv1.HealthCheck_TYPE_HTTP,
		Path:           "/",
		TimeoutSeconds: 2,
	}

	cleanupContainerdService(t, engine, cfg, allocID)
	if _, _, err := engine.EnsureService(ctx, first); err != nil {
		if skippableRuntimeErr(err) {
			t.Skipf("containerd/CNI cannot start a workload: %v", err)
		}
		t.Fatalf("EnsureService(gen 3): %v", err)
	}
	firstNetNS := requirePersistedNetNS(t, engine, allocID)

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	secondMarker := "gen-4-" + allocID
	second := busyboxHTTPService(allocID, 7, 4, "10.200.9.17", ip, secondMarker)
	second.Spec.Runtime.HealthCheck = first.Spec.Runtime.HealthCheck
	status, created, err := engine.EnsureService(ctx, second)
	if err != nil {
		t.Fatalf("EnsureService(gen 4): %v", err)
	}
	if !created {
		t.Fatal("expected generation replace to create a new container")
	}
	if status.NetworkNamespacePath == "" || status.NetworkNamespacePath == firstNetNS {
		t.Fatalf("expected a new netns after replace, got %q (old %q)", status.NetworkNamespacePath, firstNetNS)
	}

	assertContainerRunning(t, ctx, engine, allocID)
	if rec := inspectTestContainer(t, ctx, engine, allocID); rec.rolloutGeneration != 4 {
		t.Fatalf("container generation = %d, want 4", rec.rolloutGeneration)
	}
	if _, err := os.Lstat(firstNetNS); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old netns path still present: %s (%v)", firstNetNS, err)
	}

	waitForWorkloadMarker(t, ctx, status.NetworkNamespacePath, ip, secondMarker)
	probe := probeServiceHealthInNamespace(ctx, status.NetworkNamespacePath, ip, second)
	if !probe.healthy {
		t.Fatalf("production netns probe failed after replace: %+v", probe)
	}
	if got := httpGetInNamespace(t, ctx, status.NetworkNamespacePath, ip, 8080, "/"); got != secondMarker {
		t.Fatalf("replaced workload served %q, want %q", got, secondMarker)
	}

	realNetNS := status.NetworkNamespacePath
	stalePath := filepath.Join(t.TempDir(), "missing-netns")
	if err := os.WriteFile(engine.(*containerdEngine).netnsFile(allocID), []byte(stalePath), 0o644); err != nil {
		t.Fatalf("stale netns path: %v", err)
	}
	t.Cleanup(func() {
		_ = engine.(*containerdEngine).persistNetNSPath(allocID, realNetNS)
	})
	closed := probeServiceHealthInNamespace(ctx, stalePath, ip, second)
	if closed.healthy {
		t.Fatal("readiness succeeded after the netns path went stale; host-network fallback is forbidden")
	}
	if !strings.Contains(closed.failureReason, "workload network namespace") &&
		!strings.Contains(closed.failureReason, "open workload network namespace") {
		t.Fatalf("expected fail-closed netns error, got %+v", closed)
	}

	runtime := &ContainerdRuntime{cfg: cfg, engine: engine}
	report, err := runtime.Reconcile(ctx, &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{second}})
	if err != nil {
		t.Fatalf("Reconcile after stale netns: %v", err)
	}
	if len(report.GetServices()) != 1 || report.GetServices()[0].GetHealthy() || report.GetServices()[0].GetPhase() == "Healthy" {
		t.Fatalf("expected pending readiness after stale netns, got %+v", report.GetServices())
	}
}

func newTestContainerdEngine(t *testing.T) (serviceEngine, config.AgentConfig) {
	t.Helper()
	socket := strings.TrimSpace(os.Getenv("CONTAINERD_ADDRESS"))
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("containerd socket %s is not reachable: %v", socket, err)
	}
	probe, err := containerd.New(socket)
	if err != nil {
		t.Skipf("containerd is not reachable on %s: %v", socket, err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close containerd probe: %v", err)
	}

	dataDir := t.TempDir()
	cfg := config.AgentConfig{
		Profile: config.ProfileDevelopment,
		// The underlay advertise address stays IPv6 even though workloads are dual-stack.
		Node: config.NodeConfig{
			ID:            "node-" + uniqueRuntimeID("id"),
			Name:          "runtime-test-node",
			AdvertiseAddr: "fd00:44::10",
		},
		// The engine under test never dials the control plane; these only satisfy
		// FinalizeAgent, which validates the whole agent config.
		ControlPlane: config.ControlPlaneClientConfig{
			Address: "[fd00:44::1]:8443",
			TLS: config.ClientTLSConfig{
				CAFile:         filepath.Join(dataDir, "ca.crt"),
				BootstrapToken: "runtime-test-token",
			},
		},
		Mesh:         config.MeshConfig{Host: config.HostConfig{IPv6: "fd00:44::10"}},
		Runtime: config.RuntimeConfig{
			DataDir:     dataDir,
			VolumesDir:  filepath.Join(dataDir, "volumes"),
			Snapshotter: "native",
		},
		Containerd: config.ContainerdConfig{
			Socket:    socket,
			Namespace: "platform-test-" + uniqueRuntimeID("ns"),
		},
	}
	if err := config.FinalizeAgent(&cfg); err != nil {
		t.Fatalf("FinalizeAgent: %v", err)
	}
	engine, err := newContainerdEngine(cfg)
	if err != nil {
		t.Skipf("containerd engine unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close containerd engine: %v", err)
		}
	})
	return engine, cfg
}

func busyboxHTTPService(allocationID string, identity uint32, generation int64, ipv4, ipv6, marker string) *agentv1.DesiredService {
	return &agentv1.DesiredService{
		AllocationId:             allocationID,
		ServiceId:                "svc-" + allocationID,
		EnvironmentId:            "env-" + allocationID,
		Name:                     "busybox-http",
		NetworkIdentity:          identity,
		PrivateIpv4:              ipv4,
		PrivateIpv6:              ipv6,
		DesiredSpecRevision:      1,
		DesiredRolloutGeneration: generation,
		Spec: &platformv1.ResolvedServiceSpec{
			Image: containerdTestImage,
			Runtime: &platformv1.ServiceRuntime{
				Command: []string{"sh", "-c"},
				Args: []string{
					fmt.Sprintf("printf '%%s' '%s' > /tmp/index.html && exec httpd -f -p [::]:8080 -h /tmp", marker),
				},
				Ports: []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
			},
		},
	}
}

func cleanupContainerdService(t *testing.T, engine serviceEngine, cfg config.AgentConfig, allocationID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.RemoveService(ctx, allocationID); err != nil {
			t.Errorf("cleanup RemoveService(%s): %v", allocationID, err)
		}
		if leftover := remainingServiceArtifacts(ctx, engine, cfg, allocationID, ""); leftover != "" {
			t.Errorf("leftover after cleanup of %s: %s", allocationID, leftover)
		}
	})
}

func assertContainerRunning(t *testing.T, ctx context.Context, engine serviceEngine, allocationID string) {
	t.Helper()
	rec := inspectTestContainer(t, ctx, engine, allocationID)
	if !rec.running {
		t.Fatalf("container %s is not running", containerName(allocationID))
	}
}

func inspectTestContainer(t *testing.T, ctx context.Context, engine serviceEngine, allocationID string) inspectRecord {
	t.Helper()
	ce := engine.(*containerdEngine)
	rec, exists, err := ce.inspect(ce.namespaced(ctx), containerName(allocationID))
	if err != nil {
		t.Fatalf("inspect %s: %v", allocationID, err)
	}
	if !exists {
		t.Fatalf("containerd has no container %s", containerName(allocationID))
	}
	return rec
}

func requirePersistedNetNS(t *testing.T, engine serviceEngine, allocationID string) string {
	t.Helper()
	path, ok := engine.(*containerdEngine).netnsPath(allocationID)
	if !ok {
		t.Fatal("persisted netns path is missing")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("persisted netns path %s: %v", path, err)
	}
	return path
}

func waitForWorkloadMarker(t *testing.T, ctx context.Context, netnsPath, ip, marker string) {
	t.Helper()
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		body, err := httpGetInNamespaceErr(ctx, netnsPath, ip, 8080, "/")
		return err == nil && body == marker, nil
	}); err != nil {
		t.Fatalf("wait for marker %q: %v", marker, err)
	}
}

func httpGetInNamespace(t *testing.T, ctx context.Context, netnsPath, ip string, port int, path string) string {
	t.Helper()
	var body string
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		got, err := httpGetInNamespaceErr(ctx, netnsPath, ip, port, path)
		if err != nil {
			return false, nil
		}
		body = got
		return true, nil
	}); err != nil {
		t.Fatalf("HTTP GET from netns %s to %s%s: %v", netnsPath, net.JoinHostPort(ip, strconv.Itoa(port)), path, err)
	}
	return body
}

func httpGetInNamespaceErr(ctx context.Context, netnsPath, ip string, port int, path string) (string, error) {
	client := healthHTTPClient(2*time.Second, workloadNamespaceDialer(netnsPath))
	// JoinHostPort brackets IPv6 literals and leaves IPv4 bare; formatting "[%s]"
	// unconditionally produces an invalid URL for IPv4 workload addresses.
	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return string(body), nil
}

func assertServiceTornDown(t *testing.T, ctx context.Context, engine serviceEngine, cfg config.AgentConfig, allocationID, netnsPath string) {
	t.Helper()
	if leftover := remainingServiceArtifacts(ctx, engine, cfg, allocationID, netnsPath); leftover != "" {
		t.Fatalf("RemoveService left artifacts: %s", leftover)
	}
}

func remainingServiceArtifacts(ctx context.Context, engine serviceEngine, cfg config.AgentConfig, allocationID, netnsPath string) string {
	ce := engine.(*containerdEngine)
	var leftover []string
	_, err := ce.client.LoadContainer(namespaces.WithNamespace(ctx, cfg.Containerd.Namespace), containerName(allocationID))
	if err == nil {
		leftover = append(leftover, "container "+containerName(allocationID))
	} else if !errdefs.IsNotFound(err) {
		leftover = append(leftover, "load container: "+err.Error())
	}
	if path, ok := ce.netnsPath(allocationID); ok {
		leftover = append(leftover, "netns pointer "+path)
	}
	if netnsPath != "" {
		if _, err := os.Lstat(netnsPath); err == nil {
			leftover = append(leftover, "netns mount "+netnsPath)
		}
	}
	hostsPath, err := runtimeChildPath(filepath.Join(cfg.Runtime.DataDir, serviceHostsDir), "allocation ID", allocationID)
	if err == nil {
		if _, err := os.Lstat(hostsPath); err == nil {
			leftover = append(leftover, "hosts file "+hostsPath)
		}
	}
	return strings.Join(leftover, "; ")
}

func skippableRuntimeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "operation not permitted") ||
		os.Geteuid() != 0 && strings.Contains(msg, "create netns")
}

func uniqueRuntimeID(prefix string) string {
	return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
}
