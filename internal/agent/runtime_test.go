package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type fakeEngine struct {
	ensured []string
	removed []string
	status  map[string]serviceStatus
	created map[string]bool
	stopped map[string]bool
}

func TestPersistDesiredServiceDoesNotRewriteUnchangedState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runtime := &ContainerdRuntime{cfg: config.AgentConfig{Runtime: config.RuntimeConfig{DataDir: dir}}}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &agentv1.DesiredService{
		AllocationId:        "alloc-1",
		ServiceId:           "svc-1",
		DesiredSpecRevision: 1,
	}
	if err := runtime.persistDesiredService(svc); err != nil {
		t.Fatalf("initial persistDesiredService: %v", err)
	}
	path := filepath.Join(dir, "desired", "alloc-1.json")
	sentinel := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, sentinel, sentinel); err != nil {
		t.Fatalf("set sentinel mtime: %v", err)
	}
	if err := runtime.persistDesiredService(svc); err != nil {
		t.Fatalf("unchanged persistDesiredService: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(sentinel) {
		t.Fatalf("unchanged state was rewritten: mtime = %v, want %v", info.ModTime(), sentinel)
	}
}

func (f *fakeEngine) EnsureService(_ context.Context, svc *agentv1.DesiredService) (serviceStatus, bool, error) {
	f.ensured = append(f.ensured, svc.GetAllocationId())
	status := f.status[svc.GetAllocationId()]
	if f.stopped[svc.GetAllocationId()] {
		status.Running = false
	} else {
		status.Running = true
	}
	return status, f.created[svc.GetAllocationId()], nil
}

func (f *fakeEngine) RemoveService(_ context.Context, allocationID string) error {
	f.removed = append(f.removed, allocationID)
	return nil
}

func (f *fakeEngine) SetLogSink(LogSink) {}

func (f *fakeEngine) Close() error { return nil }

func TestContainerdRuntimeReconcilePersistsDesiredStateAndCallsEngine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{
		status: map[string]serviceStatus{"alloc-1": {
			AppliedSpecRevision:      2,
			AppliedRolloutGeneration: 2,
			AllocationIP:             "fd00::10",
			NetworkNamespacePath:     "/run/netns/alloc-1",
		}},
		created: map[string]bool{"alloc-1": true},
	}
	var probedNamespace string
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{
			Runtime: config.RuntimeConfig{
				DataDir:    dir,
				VolumesDir: filepath.Join(dir, "volumes"),
			},
		},
		engine: engine,
		probeHealth: func(_ context.Context, namespacePath, _ string, _ *agentv1.DesiredService) serviceHealthProbe {
			probedNamespace = namespacePath
			return serviceHealthProbe{configured: true, healthy: true}
		},
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := &agentv1.DesiredNodeState{
		AgentId:  "node-1",
		Revision: 2,
		Volumes:  []*agentv1.DesiredVolume{{VolumeId: "vol-1", Name: "data"}},
		Services: []*agentv1.DesiredService{{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			DesiredSpecRevision:      2,
			DesiredRolloutGeneration: 2,
			PrivateIpv6:              "fd00::10",
			Spec: &platformv1.ResolvedServiceSpec{
				Image: "example.com/test@sha256:abc",
				Runtime: &platformv1.ServiceRuntime{
					Ports:       []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
					HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
				},
			},
		}},
	}
	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(engine.ensured) != 1 || engine.ensured[0] != "alloc-1" {
		t.Fatalf("unexpected ensured services: %#v", engine.ensured)
	}
	if _, err := os.Stat(filepath.Join(dir, "desired", "alloc-1.json")); err != nil {
		t.Fatalf("expected desired-state file: %v", err)
	}
	if report.Services[0].Phase != "Healthy" {
		t.Fatalf("expected Healthy phase, got %q", report.Services[0].Phase)
	}
	if probedNamespace != "/run/netns/alloc-1" {
		t.Fatalf("probe did not receive workload namespace path: %q", probedNamespace)
	}
}

func TestContainerdRuntimeKeepsRolloutPendingWhileHTTPReadinessFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{
		status:  map[string]serviceStatus{"alloc-1": {AllocationIP: "fd00::10", AppliedSpecRevision: 1, AppliedRolloutGeneration: 1}},
		created: map[string]bool{"alloc-1": false},
	}
	runtime := &ContainerdRuntime{
		cfg:    config.AgentConfig{Runtime: config.RuntimeConfig{DataDir: dir, VolumesDir: filepath.Join(dir, "volumes")}},
		engine: engine,
		probeHealth: func(context.Context, string, string, *agentv1.DesiredService) serviceHealthProbe {
			return serviceHealthProbe{configured: true, failureReason: "HTTP port 8080: status 302"}
		},
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{
		AgentId: "node-1",
		Services: []*agentv1.DesiredService{{
			AllocationId: "alloc-1",
			ServiceId:    "svc-1",
			Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{
				Ports:       []*platformv1.ServiceRuntimePort{{Port: 8080}},
				HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	condition := report.GetServices()[0]
	if condition.GetHealthy() || condition.GetPhase() != "Starting" {
		t.Fatalf("expected failed readiness check to keep rollout starting, got %+v", condition)
	}
	if !strings.Contains(condition.GetMessage(), "status 302") {
		t.Fatalf("expected redirect status failure reason, got %q", condition.GetMessage())
	}
}

func TestHealthHTTPClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	requests := 0
	client := healthHTTPClient(0, nil)
	client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://example.invalid/redirected"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	resp, err := client.Get("http://127.0.0.1/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || requests != 1 {
		t.Fatalf("client followed redirect: status=%d requests=%d", resp.StatusCode, requests)
	}
}

func TestHTTPHealthProbeUsesProvidedDialer(t *testing.T) {
	t.Parallel()

	dialed := make(chan struct{}, 1)
	dial := func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		dialed <- struct{}{}
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
		}()
		return client, nil
	}
	err := probeHealthCheck(context.Background(), "fd00::10", 8080, &platformv1.HealthCheck{
		Type: platformv1.HealthCheck_TYPE_HTTP,
		Path: "/healthz",
	}, dial)
	if err != nil {
		t.Fatalf("probeHealthCheck: %v", err)
	}
	select {
	case <-dialed:
	default:
		t.Fatal("HTTP probe bypassed the provided dialer")
	}
}

func TestProbeServiceHealthReportsNamespaceDialFailure(t *testing.T) {
	t.Parallel()

	result := probeServiceHealthWithDialer(context.Background(), "fd00::10", &agentv1.DesiredService{
		Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{
			Ports:       []*platformv1.ServiceRuntimePort{{Port: 8080}},
			HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
		}},
	}, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("workload network namespace is unavailable")
	})
	if result.healthy || !result.configured {
		t.Fatalf("expected configured failed probe, got %+v", result)
	}
	if !strings.Contains(result.failureReason, "workload network namespace is unavailable") {
		t.Fatalf("missing namespace failure reason: %q", result.failureReason)
	}
}

func TestContainerdRuntimeSkipsHealthProbeWhenNoCheckIsConfigured(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{Runtime: config.RuntimeConfig{
			DataDir: dir, VolumesDir: filepath.Join(dir, "volumes"),
		}},
		engine: &fakeEngine{
			status: map[string]serviceStatus{"alloc-1": {
				AppliedSpecRevision: 1, AppliedRolloutGeneration: 1,
			}},
			created: map[string]bool{"alloc-1": true},
		},
		probeHealth: func(context.Context, string, string, *agentv1.DesiredService) serviceHealthProbe {
			t.Fatal("health probe called without a configured check")
			return serviceHealthProbe{}
		},
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{
		Services: []*agentv1.DesiredService{{
			AllocationId: "alloc-1",
			ServiceId:    "svc-1",
			Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{
				Ports: []*platformv1.ServiceRuntimePort{{Port: 8080}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	condition := report.GetServices()[0]
	if !condition.GetHealthy() || condition.GetPhase() != "Healthy" {
		t.Fatalf("expected running process to make deployment healthy immediately, got %+v", condition)
	}
	if len(condition.GetHealthyPorts()) != 1 || condition.GetHealthyPorts()[0] != 8080 {
		t.Fatalf("expected declared port to become routable, got %+v", condition.GetHealthyPorts())
	}
}

func TestContainerdRuntimeStopsCheckingAfterRolloutBecomesReady(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	checks := 0
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{Runtime: config.RuntimeConfig{
			DataDir: dir, VolumesDir: filepath.Join(dir, "volumes"),
		}},
		engine: &fakeEngine{
			status: map[string]serviceStatus{"alloc-1": {
				AppliedSpecRevision: 1, AppliedRolloutGeneration: 1,
			}},
			created: map[string]bool{},
		},
		probeHealth: func(context.Context, string, string, *agentv1.DesiredService) serviceHealthProbe {
			checks++
			if checks == 1 {
				return serviceHealthProbe{configured: true, failureReason: "HTTP port 8080: status 503"}
			}
			return serviceHealthProbe{configured: true, healthy: true}
		},
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := &agentv1.DesiredService{
		AllocationId:             "alloc-1",
		ServiceId:                "svc-1",
		DesiredRolloutGeneration: 1,
		Spec: &platformv1.ResolvedServiceSpec{Runtime: &platformv1.ServiceRuntime{
			Ports:       []*platformv1.ServiceRuntimePort{{Port: 8080}},
			HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: "/healthz"},
		}},
	}
	state := &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{service}}
	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if report.GetServices()[0].GetHealthy() || report.GetServices()[0].GetPhase() != "Starting" {
		t.Fatalf("expected rollout to wait for readiness, got %+v", report.GetServices()[0])
	}
	for range 2 {
		report, err = runtime.Reconcile(context.Background(), state)
		if err != nil {
			t.Fatalf("ready Reconcile: %v", err)
		}
		if !report.GetServices()[0].GetHealthy() {
			t.Fatalf("expected ready rollout, got %+v", report.GetServices()[0])
		}
	}
	if checks != 2 {
		t.Fatalf("expected checks to stop after readiness passes, got %d", checks)
	}

	service.DesiredRolloutGeneration = 2
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("Reconcile new rollout: %v", err)
	}
	if checks != 3 {
		t.Fatalf("expected a new rollout to run readiness again, got %d checks", checks)
	}
}

func TestHTTPReadinessRequiresStatus200(t *testing.T) {
	t.Parallel()

	dial := func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(server, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
		}()
		return client, nil
	}
	err := probeHealthCheck(context.Background(), "fd00::10", 8080, &platformv1.HealthCheck{
		Type: platformv1.HealthCheck_TYPE_HTTP,
		Path: "/healthz",
	}, dial)
	if err == nil || !strings.Contains(err.Error(), "status 204") {
		t.Fatalf("expected 204 to remain unready, got %v", err)
	}
}

func TestCPUQuotaForMillis(t *testing.T) {
	t.Parallel()

	tests := []struct {
		millis int64
		quota  int64
	}{{millis: 250, quota: 25_000}, {millis: 500, quota: 50_000}, {millis: 2000, quota: 200_000}}
	for _, tt := range tests {
		quota, period := cpuCFSForMillis(tt.millis)
		if quota != tt.quota || period != 100_000 {
			t.Fatalf("cpuCFSForMillis(%d) = (%d, %d), want (%d, 100000)", tt.millis, quota, period, tt.quota)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestContainerdRuntimeReconcileRemovesStaleService(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{status: map[string]serviceStatus{}, created: map[string]bool{}}
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{
			Runtime: config.RuntimeConfig{
				DataDir:    dir,
				VolumesDir: filepath.Join(dir, "volumes"),
			},
		},
		engine: engine,
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desired", "old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes", "old-vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{AgentId: "node-1"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(engine.removed) != 1 || engine.removed[0] != "old" {
		t.Fatalf("unexpected removed services: %#v", engine.removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "volumes", "old-vol")); !os.IsNotExist(err) {
		t.Fatalf("expected stale volume dir removal, stat err=%v", err)
	}
}

func TestContainerdRuntimeRejectsIDsThatEscapeRuntimeDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{status: map[string]serviceStatus{}, created: map[string]bool{}}
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{Runtime: config.RuntimeConfig{
			DataDir: dir, VolumesDir: filepath.Join(dir, "volumes"),
		}},
		engine: engine,
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{
		AgentId: "node-1",
		Volumes: []*agentv1.DesiredVolume{{VolumeId: "../../outside"}},
		Services: []*agentv1.DesiredService{{
			AllocationId: "../outside",
			VolumeId:     "/host",
		}},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.Volumes[0].Phase != "Error" || report.Services[0].Phase != "Error" {
		t.Fatalf("expected unsafe IDs to be rejected: %+v", report)
	}
	if len(engine.ensured) != 0 {
		t.Fatalf("unsafe service reached engine: %#v", engine.ensured)
	}
	if _, err := os.Stat(filepath.Join(dir, "outside")); !os.IsNotExist(err) {
		t.Fatalf("unsafe path was created: %v", err)
	}
}
