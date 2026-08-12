package localteststack

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartManagedRegistryAuthProxyPublishesLocalhostRealm(t *testing.T) {
	t.Parallel()

	runner := &recordingDockerRunner{}
	// Fake runner never opens a listener; use a short deadline so readiness fails fast.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := StartManagedRegistryAuthProxy(ctx, LocalRegistryAuthProxyConfig{
		ContainerName: "test-registry-auth-proxy",
		UpstreamHost:  "host.docker.internal",
		UpstreamPort:  9444,
		HostPort:      18080,
		Image:         "alpine/socat:test",
	}, runner)
	if err == nil {
		t.Fatal("expected readiness failure with fake runner")
	}
	var runArgs []string
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "run" {
			runArgs = call
			break
		}
	}
	if len(runArgs) == 0 {
		t.Fatalf("expected docker run, got %#v", runner.calls)
	}
	joined := strings.Join(runArgs, " ")
	for _, want := range []string{
		"--publish 127.0.0.1:18080:18080",
		"--add-host host.docker.internal:host-gateway",
		"TCP-LISTEN:18080,fork,reuseaddr",
		"TCP:host.docker.internal:9444",
		"alpine/socat:test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker run missing %q\nargs: %s", want, joined)
		}
	}
}

func TestManagedRegistryAuthProxyTokenRealmBaseURL(t *testing.T) {
	t.Parallel()
	proxy := &ManagedRegistryAuthProxy{cfg: LocalRegistryAuthProxyConfig{HostPort: 18080}}
	if got := proxy.TokenRealmBaseURL(); got != "http://localhost:18080" {
		t.Fatalf("unexpected realm base URL %q", got)
	}
}

type recordingDockerRunner struct {
	calls [][]string
}

func (r *recordingDockerRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	copied := append([]string(nil), args...)
	r.calls = append(r.calls, copied)
	return nil, nil
}
