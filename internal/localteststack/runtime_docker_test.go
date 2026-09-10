package localteststack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDockerRuntimeReconcileCreatesContainerAndReportsDNSEndpoint(t *testing.T) {
	t.Parallel()

	var readinessRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		readinessRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	host, port := splitHostPort(t, server.Listener.Addr().String())

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runner.imageExists = true
	runner.onRun = func(args []string) {
		if len(args) >= 2 && args[0] == "run" && args[1] == "--detach" {
			runner.containers["localteststack-svc-alloc-1"] = dockerContainerInspect{
				State: dockerContainerState{Running: true},
				Config: struct {
					Image  string            `json:"Image"`
					Labels map[string]string `json:"Labels"`
				}{
					Image: "ghcr.io/demo/echo:latest",
					Labels: map[string]string{
						"platform.runtime":                    localRuntimeManagedBy,
						"platform.allocation_id":              "alloc-1",
						"platform.service_id":                 "svc-1",
						"platform.desired_spec_revision":      "2",
						"platform.desired_rollout_generation": "3",
						"mesh.environment_id":                 "7",
						"mesh.ipv4":                           "10.200.0.2",
						"mesh.ipv6":                           "fd00:200::2",
						"platform.internal_hostname":          "accurate-reflection.mesh.internal",
					},
				},
				NetworkSettings: fakeNetworkSettings(host, port, 8080),
			}
		}
	}

	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}

	state := &agentv1.DesiredNodeState{
		AgentId: "node-1",
		Volumes: []*agentv1.DesiredVolume{{VolumeId: "vol-1", Name: "data"}},
		Services: []*agentv1.DesiredService{{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			EnvironmentId:            "environment-1",
			NetworkIdentity:          7,
			Name:                     "echo",
			InternalHostname:         "accurate-reflection.mesh.internal",
			PrivateIpv4:              "10.200.0.2",
			PrivateIpv6:              "fd00:200::2",
			DesiredSpecRevision:      2,
			DesiredRolloutGeneration: 3,
			VolumeId:                 "vol-1",
			Spec: &platformv1.ResolvedServiceSpec{
				Image: "ghcr.io/demo/echo:latest",
				Runtime: &platformv1.ServiceRuntime{
					Ports: []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
					HealthCheck: &platformv1.HealthCheck{
						Type: platformv1.HealthCheck_TYPE_HTTP,
						Path: "/",
					},
				},
			},
		}},
	}

	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Phase != "Ready" {
		t.Fatalf("unexpected volume report %+v", report.Volumes)
	}
	if len(report.Services) != 1 {
		t.Fatalf("unexpected service report %+v", report.Services)
	}
	service := report.Services[0]
	if !service.Healthy || service.Phase != "Healthy" {
		t.Fatalf("expected healthy service, got %+v", service)
	}
	if service.AllocationIpv4 != "10.200.0.2" || service.AllocationIpv6 != "fd00:200::2" {
		t.Fatalf("unexpected allocation addresses %q, %q", service.AllocationIpv4, service.AllocationIpv6)
	}
	if len(service.HealthyIpv4Ports) != 1 || service.HealthyIpv4Ports[0] != 8080 ||
		len(service.HealthyIpv6Ports) != 1 || service.HealthyIpv6Ports[0] != 8080 {
		t.Fatalf("unexpected healthy ports %+v", service)
	}
	if _, err := runtime.Reconcile(context.Background(), state); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := readinessRequests.Load(); got != 1 {
		t.Fatalf("expected readiness endpoint to be called once, got %d", got)
	}
	runArgs := runner.firstCommand("run")
	assertArgContains(t, runArgs, "--network", runtime.environmentNetworkName("environment-1"))
	assertArgContains(t, runArgs, "--network-alias", "accurate-reflection.mesh.internal")
	assertArgContains(t, runArgs, "--network-alias", "accurate-reflection")
	if !runner.hasCommand("network", "connect", "mesh-local", "localteststack-svc-alloc-1") {
		t.Fatalf("expected ingress network connection, got commands %+v", runner.commands)
	}
	assertArgContains(t, runArgs, "--mount", "type=bind,src="+filepath.Join(dir, "volumes", "vol-1")+",dst="+localRuntimeVolumeMount)
	assertArgContains(t, runArgs, "--publish", "127.0.0.1::8080")
	assertArgContains(t, runArgs, "--security-opt", "no-new-privileges")
	assertArgContains(t, runArgs, "--cap-drop", "ALL")
	for _, capability := range []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID", "SETPCAP", "NET_BIND_SERVICE", "KILL"} {
		assertArgContains(t, runArgs, "--cap-add", capability)
	}
	assertArgContains(t, runArgs, "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m")
	if slices.Contains(runArgs, "--user") || slices.Contains(runArgs, "--read-only") {
		t.Fatalf("production sandbox overrode image USER or writable overlay root: %v", runArgs)
	}
	for index, arg := range runArgs {
		if arg == "--privileged" || strings.HasPrefix(arg, "--device") || arg == "--pid=host" || arg == "--network=host" {
			t.Fatalf("customer isolation flags leaked into docker run: %v", runArgs)
		}
		if arg == "--cap-add" && index+1 < len(runArgs) && slices.Contains([]string{"SYS_ADMIN", "NET_ADMIN", "NET_RAW", "BPF"}, runArgs[index+1]) {
			t.Fatalf("dangerous capability leaked into docker run: %v", runArgs)
		}
	}
}

func TestDockerRuntimePullUsesEphemeralScopedCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	var authConfig map[string]any
	runner.onRun = func(args []string) {
		if len(args) < 4 || args[0] != "--config" || args[2] != "pull" {
			return
		}
		raw, err := os.ReadFile(filepath.Join(args[1], "config.json"))
		if err != nil {
			t.Fatalf("read ephemeral docker config: %v", err)
		}
		if err := json.Unmarshal(raw, &authConfig); err != nil {
			t.Fatalf("decode ephemeral docker config: %v", err)
		}
	}
	runtime := &DockerRuntime{cfg: DockerRuntimeConfig{DataDir: dir}, runner: runner}
	image := "localhost:5000/mesh/project/build/service@sha256:" + strings.Repeat("a", 64)
	if err := runtime.ensureImage(context.Background(), image, "pull-agent", "scoped-secret"); err != nil {
		t.Fatal(err)
	}
	if authConfig == nil {
		t.Fatal("authenticated pull did not receive a Docker config")
	}
	auths := authConfig["auths"].(map[string]any)
	entry := auths["localhost:5000"].(map[string]any)
	if entry["auth"] != "cHVsbC1hZ2VudDpzY29wZWQtc2VjcmV0" {
		t.Fatalf("unexpected encoded auth %v", entry["auth"])
	}
}

func TestDockerRuntimeReconcileWithoutHealthCheckIsReadyAfterStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runner.imageExists = true
	runner.onRun = func(args []string) {
		if len(args) >= 2 && args[0] == "run" && args[1] == "--detach" {
			runner.containers["localteststack-svc-alloc-1"] = dockerContainerInspect{
				State: dockerContainerState{Running: true},
				Config: struct {
					Image  string            `json:"Image"`
					Labels map[string]string `json:"Labels"`
				}{
					Image: "ghcr.io/demo/echo:latest",
					Labels: map[string]string{
						"platform.runtime":                    localRuntimeManagedBy,
						"platform.allocation_id":              "alloc-1",
						"platform.service_id":                 "svc-1",
						"platform.desired_spec_revision":      "2",
						"platform.desired_rollout_generation": "3",
					},
				},
				NetworkSettings: fakeNetworkSettings("127.0.0.1", "65530", 8080),
			}
		}
	}

	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}

	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{
		AgentId: "node-1",
		Services: []*agentv1.DesiredService{{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			Name:                     "echo",
			PrivateIpv4:              "10.200.0.2",
			PrivateIpv6:              "fd00:200::2",
			DesiredSpecRevision:      2,
			DesiredRolloutGeneration: 3,
			Spec: &platformv1.ResolvedServiceSpec{
				Image: "ghcr.io/demo/echo:latest",
				Runtime: &platformv1.ServiceRuntime{
					Ports: []*platformv1.ServiceRuntimePort{{Port: 8080, Primary: true}},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, want := report.Services[0].AllocationIpv4, "10.200.0.2"; got != want {
		t.Fatalf("AllocationIpv4 = %q, want assigned %q", got, want)
	}
	if got, want := report.Services[0].AllocationIpv6, "fd00:200::2"; got != want {
		t.Fatalf("AllocationIpv6 = %q, want assigned %q", got, want)
	}
	if got := report.Services[0].Phase; got != "Healthy" {
		t.Fatalf("expected Healthy phase, got %q", got)
	}
	if !report.Services[0].Healthy {
		t.Fatalf("expected running workload to be ready without a check %+v", report.Services[0])
	}
	if len(report.Services[0].HealthyIpv4Ports) != 1 || report.Services[0].HealthyIpv4Ports[0] != 8080 ||
		len(report.Services[0].HealthyIpv6Ports) != 1 || report.Services[0].HealthyIpv6Ports[0] != 8080 {
		t.Fatalf("expected declared port to be routable %+v", report.Services[0])
	}
}

func TestDockerRuntimeReconcileRemovesStaleContainerAndVolume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desired", "stale.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes", "stale-vol"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{AgentId: "node-1"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !runner.hasCommand("rm", "--force", "localteststack-svc-stale") {
		t.Fatalf("expected stale container removal, got commands %+v", runner.commands)
	}
	if _, err := os.Stat(filepath.Join(dir, "volumes", "stale-vol")); !os.IsNotExist(err) {
		t.Fatalf("expected stale volume removal, stat err=%v", err)
	}
}

func TestDockerRuntimeRecoveryPreservesUnknownResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desired", "unknown.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownVolume := filepath.Join(dir, "volumes", "unknown-vol")
	if err := os.MkdirAll(unknownVolume, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.ReconcileWithCleanup(context.Background(), &agentv1.DesiredNodeState{AgentId: "node-1"}, false); err != nil {
		t.Fatalf("ReconcileWithCleanup: %v", err)
	}
	if runner.hasCommand("rm", "--force", "localteststack-svc-unknown") {
		t.Fatalf("recovery removed an unowned container: %+v", runner.commands)
	}
	if _, err := os.Stat(unknownVolume); err != nil {
		t.Fatalf("recovery removed an unowned volume: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "desired", "unknown.json")); err != nil {
		t.Fatalf("Close removed durable runtime state: %v", err)
	}
}

func TestDockerRuntimeDiscoversStableRuntimeResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runner.containers["container-id"] = dockerContainerInspect{
		ID:   "container-id",
		Name: "/localteststack-svc-alloc-1",
		Config: struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		}{Labels: map[string]string{
			localRuntimeLabel:        localRuntimeManagedBy,
			"platform.allocation_id": "alloc-1",
		}},
	}
	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}
	volumePath := filepath.Join(dir, "volumes", "vol-1")
	if err := os.MkdirAll(volumePath, 0o755); err != nil {
		t.Fatal(err)
	}

	resources, err := runtime.DiscoverRuntimeResources(context.Background())
	if err != nil {
		t.Fatalf("DiscoverRuntimeResources: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("resources = %+v, want allocation and volume", resources)
	}
	if resources[0].AllocationID != "alloc-1" || resources[0].RuntimeID != "localteststack-svc-alloc-1" {
		t.Fatalf("allocation resource = %+v", resources[0])
	}
	if resources[1].VolumeID != "vol-1" || resources[1].RuntimeID != volumePath {
		t.Fatalf("volume resource = %+v", resources[1])
	}
}

func TestDockerRuntimePrunesDiscoveredContainerWithoutSidecarState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	container := dockerContainerInspect{Name: "/localteststack-svc-stale"}
	container.Config.Labels = map[string]string{
		localRuntimeLabel:        localRuntimeManagedBy,
		"platform.allocation_id": "stale",
	}
	runner.containers["container-id"] = container
	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir: dir, VolumesDir: filepath.Join(dir, "volumes"), DockerNetwork: "mesh-local",
		Runner: runner, ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{AgentId: "node-1"}); err != nil {
		t.Fatal(err)
	}
	if !runner.hasCommand("rm", "--force", "localteststack-svc-stale") {
		t.Fatalf("expected discovered stale container removal, got %+v", runner.commands)
	}
}

func TestDockerRuntimeDrainSendsSIGTERMThenForceRemovesAfterDeadline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runner := newFakeDockerRunner(t)
	runner.containers["localteststack-svc-alloc-1"] = dockerContainerInspect{
		State: dockerContainerState{Running: true},
	}
	runtime, err := NewDockerRuntime(DockerRuntimeConfig{
		DataDir:            dir,
		VolumesDir:         filepath.Join(dir, "volumes"),
		DockerNetwork:      "mesh-local",
		Runner:             runner,
		ContainerNamePrefx: "localteststack-svc",
	})
	if err != nil {
		t.Fatalf("NewDockerRuntime: %v", err)
	}

	svc := &agentv1.DesiredService{
		AllocationId:             "alloc-1",
		ServiceId:                "svc-1",
		DesiredSpecRevision:      1,
		DesiredRolloutGeneration: 1,
		Intent:                   agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN,
		DrainDeadline:            timestamppb.New(time.Now().Add(time.Minute)),
		Spec:                     &platformv1.ResolvedServiceSpec{Image: "ghcr.io/demo/echo:latest"},
	}
	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{svc}})
	if err != nil {
		t.Fatalf("Reconcile before deadline: %v", err)
	}
	if report.Services[0].Phase != "Draining" {
		t.Fatalf("phase = %q, want Draining", report.Services[0].Phase)
	}
	if !runner.hasCommand("kill", "--signal", "TERM", "localteststack-svc-alloc-1") {
		t.Fatalf("expected SIGTERM, got %+v", runner.commands)
	}
	if runner.hasCommand("rm", "--force", "localteststack-svc-alloc-1") {
		t.Fatalf("force-removed before deadline: %+v", runner.commands)
	}

	svc.DrainDeadline = timestamppb.New(time.Now().Add(-time.Second))
	report, err = runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{Services: []*agentv1.DesiredService{svc}})
	if err != nil {
		t.Fatalf("Reconcile after deadline: %v", err)
	}
	if report.Services[0].Phase != "Drained" || !strings.Contains(report.Services[0].Message, "force killed") {
		t.Fatalf("expected force kill after deadline, got %+v", report.Services[0])
	}
	if !runner.hasCommand("rm", "--force", "localteststack-svc-alloc-1") {
		t.Fatalf("expected force remove after deadline, got %+v", runner.commands)
	}
}

func TestDockerRuntimeInspectContainerTreatsLowercaseNoSuchObjectAsMissing(t *testing.T) {
	t.Parallel()

	runtime := &DockerRuntime{
		runner: fakeMissingInspectRunner{},
	}
	_, exists, err := runtime.inspectContainer(context.Background(), "localteststack-svc-missing")
	if err != nil {
		t.Fatalf("inspectContainer: %v", err)
	}
	if exists {
		t.Fatal("expected missing container to report exists=false")
	}
}

type fakeDockerRunner struct {
	t           *testing.T
	imageExists bool
	commands    [][]string
	containers  map[string]dockerContainerInspect
	onRun       func(args []string)
}

type fakeMissingInspectRunner struct{}

func (fakeMissingInspectRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "inspect" {
		return nil, fmt.Errorf("docker inspect %s: error: no such object: %s", args[1], args[1])
	}
	return nil, fmt.Errorf("unexpected docker command: %v", args)
}

func newFakeDockerRunner(t *testing.T) *fakeDockerRunner {
	t.Helper()
	return &fakeDockerRunner{
		t:          t,
		containers: make(map[string]dockerContainerInspect),
	}
}

func (f *fakeDockerRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.commands = append(f.commands, append([]string(nil), args...))
	if f.onRun != nil {
		f.onRun(args)
	}
	switch {
	case len(args) >= 2 && args[0] == "ps":
		ids := make([]string, 0, len(f.containers))
		for id, container := range f.containers {
			if container.Config.Labels[localRuntimeLabel] == localRuntimeManagedBy {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return []byte(strings.Join(ids, "\n")), nil
	case len(args) >= 3 && args[0] == "network" && args[1] == "inspect":
		return nil, fmt.Errorf("not found")
	case len(args) >= 3 && args[0] == "network" && args[1] == "create":
		return []byte("mesh-local"), nil
	case len(args) >= 4 && args[0] == "network" && args[1] == "connect":
		return []byte("connected"), nil
	case len(args) >= 3 && args[0] == "image" && args[1] == "inspect":
		if f.imageExists {
			return []byte("{}"), nil
		}
		return nil, fmt.Errorf("not found")
	case len(args) >= 2 && args[0] == "pull":
		f.imageExists = true
		return []byte("pulled"), nil
	case len(args) >= 4 && args[0] == "--config" && args[2] == "pull":
		f.imageExists = true
		return []byte("pulled"), nil
	case len(args) >= 2 && args[0] == "inspect":
		container, ok := f.containers[args[1]]
		if !ok {
			return nil, fmt.Errorf("No such object: %s", args[1])
		}
		return json.Marshal([]dockerContainerInspect{container})
	case len(args) >= 2 && args[0] == "kill":
		return []byte("killed"), nil
	case len(args) >= 2 && args[0] == "rm":
		delete(f.containers, args[len(args)-1])
		return []byte("removed"), nil
	case len(args) >= 2 && args[0] == "run":
		return []byte("started"), nil
	default:
		f.t.Fatalf("unexpected docker command: %v", args)
		return nil, nil
	}
}

func (f *fakeDockerRunner) firstCommand(name string) []string {
	for _, command := range f.commands {
		if len(command) > 0 && command[0] == name {
			return command
		}
	}
	return nil
}

func (f *fakeDockerRunner) hasCommand(items ...string) bool {
	for _, command := range f.commands {
		if len(command) != len(items) {
			continue
		}
		match := true
		for i := range items {
			if command[i] != items[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func fakeNetworkSettings(hostIP, hostPort string, containerPort int32) struct {
	Networks map[string]struct {
		IPAddress         string `json:"IPAddress"`
		GlobalIPv6Address string `json:"GlobalIPv6Address"`
	} `json:"Networks"`
	Ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	} `json:"Ports"`
} {
	return struct {
		Networks map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	}{
		Networks: map[string]struct {
			IPAddress         string `json:"IPAddress"`
			GlobalIPv6Address string `json:"GlobalIPv6Address"`
		}{
			"mesh-local": {IPAddress: "172.18.0.10", GlobalIPv6Address: "fd00:18::10"},
		},
		Ports: map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}{
			fmt.Sprintf("%d/tcp", containerPort): {{HostIP: hostIP, HostPort: hostPort}},
		},
	}
}

func splitHostPort(t *testing.T, addr string) (string, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	return host, port
}

func assertArgContains(t *testing.T, args []string, key string, want string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && strings.Contains(args[i+1], want) {
			return
		}
	}
	t.Fatalf("expected args %v to contain %s %q", args, key, want)
}
