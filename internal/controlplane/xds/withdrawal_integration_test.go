//go:build integration

package xds

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/identity/identitytest"
	"ebof-wg-mesh/internal/testutil"
)

func TestEnvoyWithdrawsHealthyEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker is unavailable: %v", err)
	}
	docker := func(args ...string) string {
		t.Helper()
		// Image pull progress goes to stderr; only stdout is the result.
		var stderr strings.Builder
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s%s", args, err, out, stderr.String())
		}
		return strings.TrimSpace(string(out))
	}
	const image = "envoyproxy/envoy:v1.36-latest"
	// EDS accepts IPs, so resolve the address that reaches this test's servers
	// from Docker on both Linux and Docker Desktop.
	lookup := docker("run", "--rm", "--add-host=host.docker.internal:host-gateway",
		"--entrypoint", "getent", image, "ahostsv4", "host.docker.internal")
	fields := strings.Fields(lookup)
	if len(fields) == 0 || net.ParseIP(fields[0]) == nil {
		t.Fatalf("invalid Docker host address: %q", lookup)
	}
	host := fields[0]
	backend := func(name string) *httptest.Server {
		t.Helper()
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, name)
		}))
		s.Listener = listener
		s.Start()
		t.Cleanup(s.Close)
		return s
	}
	old, replacement := backend("old"), backend("replacement")
	upstream := func(s *httptest.Server) string {
		return net.JoinHostPort(host, fmt.Sprint(s.Listener.Addr().(*net.TCPAddr).Port))
	}
	input := BuildInput{
		HTTPListenAddrs: []string{":18080"},
		Backends: []Backend{
			{Domain: "withdrawal.example.test", Upstream: upstream(old), AllocationID: "old"},
			{Domain: "withdrawal.example.test", Upstream: upstream(replacement), AllocationID: "replacement"},
		},
	}
	server := NewServer(ctx)
	server.Publish(ctx, mustBuild(t, input))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	pki := identitytest.New(t)
	grpcServer := server.GRPCServer(pki.Authority.XDSConfig(), pki.Authority.Revocations())
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	const nodeID = "withdrawal-test"
	dir := t.TempDir()
	identityDir := filepath.Join(dir, "identity")
	if err := WriteIdentity(identityDir, "/test/identity", pki.Material(t, identity.CallerIngress, nodeID)); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := RenderBootstrap(BootstrapConfig{
		NodeID: nodeID, IdentityDir: "/test/identity", ServerName: "controlplane",
		XDSAddresses: []string{net.JoinHostPort(host, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))},
		AdminAddress: "127.0.0.1:19000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "envoy.yaml"), []byte(bootstrap), 0o644); err != nil {
		t.Fatal(err)
	}
	container := docker("run", "--detach", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--publish", "127.0.0.1::18080", "--volume", dir+":/test:ro", image,
		"envoy", "--config-path", "/test/envoy.yaml", "--concurrency", "1", "--log-level", "warn")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if t.Failed() {
			logs, _ := exec.CommandContext(cleanupCtx, "docker", "logs", container).CombinedOutput()
			t.Logf("Envoy logs:\n%s", logs)
		}
		if out, err := exec.CommandContext(cleanupCtx, "docker", "rm", "--force", container).CombinedOutput(); err != nil {
			t.Errorf("remove Envoy: %v\n%s", err, out)
		}
	})
	url := "http://" + docker("port", container, "18080/tcp")
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request := func() (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, "", err
		}
		req.Host = "withdrawal.example.test"
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}
	seen := make(map[string]bool)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(context.Context) (bool, error) {
		code, body, err := request()
		if err == nil && code == http.StatusOK {
			seen[body] = true
		}
		return seen["old"] && seen["replacement"], nil
	}); err != nil {
		t.Fatalf("both healthy backends never served traffic: %v; seen %v; xDS %+v", err, seen, server.Status())
	}

	// Keep the withdrawn endpoint running and healthy through the ACK and
	// subsequent requests. Waiting for it to fail health checks would hide the bug.
	input.Backends = input.Backends[1:]
	withdrawn := mustBuild(t, input)
	server.Publish(ctx, withdrawn)
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(context.Context) (bool, error) {
		status := server.Status()
		return status.Nodes[nodeID].FullyApplied(withdrawn.Version, status.RequiredTypes), nil
	}); err != nil {
		t.Fatalf("Envoy did not ACK withdrawal: %v; xDS %+v", err, server.Status())
	}
	checkTraffic := func(phase string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			code, body, err := request()
			if err != nil || code != http.StatusOK || body != "replacement" {
				t.Fatalf("%s request %d: status %d, body %q, error %v", phase, i, code, body, err)
			}
		}
	}
	checkTraffic("withdrawn backend still healthy")
	old.Close()
	checkTraffic("withdrawn backend stopped")
	t.Log("both backends served traffic; after withdrawal ACK, all 200 requests reached the replacement before and after stopping the old backend")
}
