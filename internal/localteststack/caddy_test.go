package localteststack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestStartManagedIngressRunsCaddyWithNativeJSONConfig(t *testing.T) {
	t.Parallel()

	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen admin port: %v", err)
	}
	adminPort := adminListener.Addr().(*net.TCPAddr).Port
	adminServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/config/" {
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

	runner := &fakeIngressDockerRunner{}
	managed, err := StartManagedIngress(context.Background(), LocalIngressConfig{
		StateDir:      t.TempDir(),
		DockerNetwork: "mesh-local",
		ContainerName: "localteststack-caddy-test",
		PublicHost:    "platform.localtest.me",
		PublicPort:    publicPort,
		AdminPort:     adminPort,
		Image:         "caddy:2",
	}, runner)
	if err != nil {
		t.Fatalf("StartManagedIngress: %v", err)
	}
	t.Cleanup(func() {
		_ = managed.Close()
	})

	runArgs := runner.firstCommand("run")
	if len(runArgs) == 0 {
		t.Fatal("expected docker run command")
	}
	if !containsSequence(runArgs, []string{"caddy", "run", "--config", "/etc/caddy/local.json"}) {
		t.Fatalf("expected caddy run command, got %v", runArgs)
	}
	if containsArg(runArgs, "--adapter") || containsArg(runArgs, "json") {
		t.Fatalf("expected native JSON config without adapter flag, got %v", runArgs)
	}
}

func TestWriteLocalIngressBootstrapConfigRespondsUnavailableUntilSynced(t *testing.T) {
	t.Parallel()

	path, err := writeLocalIngressBootstrapConfig(LocalIngressConfig{
		StateDir:   t.TempDir(),
		PublicPort: 8080,
	})
	if err != nil {
		t.Fatalf("writeLocalIngressBootstrapConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	routes := payload["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["srv0"].(map[string]any)["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("expected one bootstrap route, got %d", len(routes))
	}
	handle := routes[0].(map[string]any)["handle"].([]any)[0].(map[string]any)
	if got := handle["handler"]; got != "static_response" {
		t.Fatalf("expected static_response handler, got %v", got)
	}
	if got := handle["status_code"]; got != float64(503) {
		t.Fatalf("expected 503 bootstrap status, got %v", got)
	}
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

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
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
