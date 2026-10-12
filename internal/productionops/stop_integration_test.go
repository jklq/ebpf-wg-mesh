//go:build integration

package productionops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvoyStopFinishesAnAdmittedRequestBeforeReturning(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker is required for the native Envoy drain test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Docker daemon is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	config := `admin:
  address:
    socket_address: { address: 0.0.0.0, port_value: 19000 }
static_resources:
  listeners:
  - name: ingress
    address:
      socket_address: { address: 0.0.0.0, port_value: 8080 }
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: ingress_http
          route_config:
            virtual_hosts:
            - name: serving
              domains: ["*"]
              routes:
              - match: { prefix: / }
                direct_response: { status: 200, body: { inline_string: finished } }
          http_filters:
          - name: envoy.filters.http.fault
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.fault.v3.HTTPFault
              delay:
                fixed_delay: 2s
                percentage: { numerator: 100, denominator: HUNDRED }
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  - name: health
    address:
      socket_address: { address: 0.0.0.0, port_value: 19001 }
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: ingress_health
          route_config:
            virtual_hosts:
            - name: health
              domains: ["*"]
              routes:
              - match: { prefix: / }
                direct_response: { status: 404 }
          http_filters:
          - name: envoy.filters.http.health_check
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.health_check.v3.HealthCheck
              pass_through_mode: false
              headers:
              - name: ":path"
                string_match: { exact: /healthz }
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
`
	file := filepath.Join(dir, "envoy.yaml")
	if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ingress-drain-%d", time.Now().UnixNano())
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Envoy fixture: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("run", "-d", "--name", name, "--publish", "127.0.0.1::19000", "--publish", "127.0.0.1::19001", "--publish", "127.0.0.1::8080", "--volume", file+":/etc/envoy/envoy.yaml:ro", "envoyproxy/envoy:v1.36-latest", "envoy", "-c", "/etc/envoy/envoy.yaml", "--concurrency", "1", "--drain-strategy", "immediate", "--drain-time-s", "1")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", name).Run() })
	origin := func(port string) string {
		return "http://" + docker("port", name, port+"/tcp")
	}
	admin, health, ingress := origin("19000"), origin("19001"), origin("8080")
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for {
		res, err := client.Get(health + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
			t.Fatal("Envoy did not become ready", err)
		}
	}
	e := envoyStop{admin: admin, client: client, replicated: true, withdrawalMin: 100 * time.Millisecond, withdrawalMax: time.Second, quiet: 50 * time.Millisecond, drain: 5 * time.Second, poll: 10 * time.Millisecond}
	finished := make(chan error, 1)
	go func() {
		res, err := client.Get(ingress)
		if err == nil {
			body, readErr := io.ReadAll(res.Body)
			res.Body.Close()
			err = readErr
			if err == nil && (res.StatusCode != http.StatusOK || string(body) != "finished") {
				err = fmt.Errorf("admitted request: %s %q", res.Status, body)
			}
		}
		finished <- err
	}()
	for {
		c, err := e.connections(ctx)
		if err == nil && c.requests == 1 {
			break
		}
		if err := sleepContext(ctx, 10*time.Millisecond); err != nil {
			t.Fatal("request was never admitted", err)
		}
	}
	drained := make(chan error, 1)
	go func() { drained <- e.run(ctx) }()
	for {
		res, err := client.Get(health + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusServiceUnavailable {
				break
			}
		}
		select {
		case err := <-drained:
			t.Fatal("drain ended before health withdrawal", err)
		default:
		}
		if err := sleepContext(ctx, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-drained:
		t.Fatal("drain ended while request was in flight", err)
	default:
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if c, err := e.connections(ctx); err != nil || c.active != 0 {
		t.Fatalf("drain returned before ingress connections finished: %+v %v", c, err)
	}
}
