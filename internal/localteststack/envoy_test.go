package localteststack

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestStartManagedIngressRunsEnvoyWithBootstrapConfig(t *testing.T) {
	t.Parallel()

	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen admin port: %v", err)
	}
	adminPort := adminListener.Addr().(*net.TCPAddr).Port
	adminServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() {
		_ = adminServer.Serve(adminListener)
	}()
	t.Cleanup(func() {
		_ = adminServer.Close()
	})

	publicListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen public port: %v", err)
	}
	publicPort := publicListener.Addr().(*net.TCPAddr).Port
	_ = publicListener.Close()

	stateDir := t.TempDir()
	runner := &fakeIngressDockerRunner{}
	managed, err := StartManagedIngress(context.Background(), LocalIngressConfig{
		StateDir:      stateDir,
		DockerNetwork: "mesh-local",
		ContainerName: "localteststack-envoy-test",
		NodeID:        "localteststack-envoy-test",
		XDSServerAddr: "host.docker.internal:18000",
		PublicHost:    "platform.localtest.me",
		PublicPort:    publicPort,
		AdminPort:     adminPort,
		Image:         "envoyproxy/envoy:v1.36-latest",
	}, runner)
	if err != nil {
		t.Fatalf("StartManagedIngress: %v", err)
	}
	t.Cleanup(func() {
		_ = managed.Close()
	})
	if err := managed.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	runArgs := runner.firstCommand("run")
	if len(runArgs) == 0 {
		t.Fatal("expected docker run command")
	}
	if !containsSequence(runArgs, []string{"envoy", "--config-path", "/etc/envoy/envoy.yaml"}) {
		t.Fatalf("expected envoy bootstrap command, got %v", runArgs)
	}
	if !containsSequence(runArgs, []string{"--volume", stateDir + "/envoy-bootstrap.yaml:/etc/envoy/envoy.yaml:ro"}) &&
		!containsVolumeMount(runArgs, "envoy-bootstrap.yaml", "/etc/envoy/envoy.yaml:ro") {
		t.Fatalf("expected bootstrap volume mount, got %v", runArgs)
	}

	raw, err := os.ReadFile(stateDir + "/envoy-bootstrap.yaml")
	if err != nil {
		t.Fatalf("ReadFile bootstrap: %v", err)
	}
	for _, want := range []string{
		"id: localteststack-envoy-test",
		"address: host.docker.internal",
		"port_value: 18000",
		"cluster_name: xds_cluster",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("bootstrap missing %q:\n%s", want, raw)
		}
	}

	for _, cfg := range []LocalIngressConfig{
		{StateDir: stateDir, DockerNetwork: "mesh-local", ContainerName: "x", NodeID: "", XDSServerAddr: "h:1", PublicHost: "h", PublicPort: 1},
		{StateDir: stateDir, DockerNetwork: "mesh-local", ContainerName: "x", NodeID: "n", XDSServerAddr: "", PublicHost: "h", PublicPort: 1},
	} {
		if _, err := StartManagedIngress(context.Background(), cfg, runner); err == nil {
			t.Fatalf("config %+v: expected validation error", cfg)
		}
	}
}

func TestManagedIngressWaitReadyRemovesContainerOnFailure(t *testing.T) {
	runner := &fakeIngressDockerRunner{}
	managed := &ManagedIngress{
		cfg:    LocalIngressConfig{ContainerName: "localteststack-envoy-test", AdminPort: 1},
		runner: runner,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := managed.WaitReady(ctx); err == nil {
		t.Fatal("expected readiness failure")
	}
	if !containsSequence(runner.firstCommand("rm"), []string{"rm", "--force", "localteststack-envoy-test"}) {
		t.Fatalf("Envoy container was not removed: %v", runner.commands)
	}
}

func containsVolumeMount(args []string, volumeSuffix, containerPath string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--volume" {
			continue
		}
		mount := args[i+1]
		if strings.HasSuffix(mount, "/"+volumeSuffix+":"+containerPath) || strings.HasSuffix(mount, volumeSuffix+":"+containerPath) {
			return true
		}
	}
	return false
}

type fakeIngressDockerRunner struct {
	networkExists bool
	commands      [][]string
}

func (f *fakeIngressDockerRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.commands = append(f.commands, append([]string(nil), args...))
	switch {
	case len(args) >= 3 && args[0] == "network" && args[1] == "inspect":
		if f.networkExists {
			return []byte(`[]`), nil
		}
		return nil, fmt.Errorf("network %s not found", args[2])
	case len(args) >= 3 && args[0] == "network" && args[1] == "create":
		f.networkExists = true
		return []byte(args[2]), nil
	case len(args) >= 2 && args[0] == "rm" && args[1] == "--force":
		return []byte("removed"), nil
	case len(args) >= 2 && args[0] == "run" && args[1] == "--detach":
		return []byte("container-id"), nil
	default:
		return nil, fmt.Errorf("unexpected docker command: %s", strings.Join(args, " "))
	}
}

func (f *fakeIngressDockerRunner) firstCommand(name string) []string {
	for _, args := range f.commands {
		if len(args) > 0 && args[0] == name {
			return args
		}
	}
	return nil
}

func containsSequence(args []string, want []string) bool {
	if len(want) == 0 || len(args) < len(want) {
		return false
	}
	for i := 0; i <= len(args)-len(want); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
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
