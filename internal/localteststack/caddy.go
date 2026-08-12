package localteststack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/testutil"
)

const (
	defaultLocalIngressImage     = "caddy:2"
	defaultLocalIngressAdminPort = 2019
)

type LocalIngressConfig struct {
	StateDir      string
	DockerNetwork string
	ContainerName string
	PublicHost    string
	PublicPort    int
	AdminPort     int
	Image         string
}

type ManagedIngress struct {
	cfg    LocalIngressConfig
	runner DockerRunner
}

func StartManagedIngress(ctx context.Context, cfg LocalIngressConfig, runner DockerRunner) (*ManagedIngress, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, fmt.Errorf("local ingress state dir is required")
	}
	if strings.TrimSpace(cfg.DockerNetwork) == "" {
		return nil, fmt.Errorf("local ingress docker network is required")
	}
	if strings.TrimSpace(cfg.ContainerName) == "" {
		return nil, fmt.Errorf("local ingress container name is required")
	}
	if strings.TrimSpace(cfg.PublicHost) == "" {
		return nil, fmt.Errorf("local ingress public host is required")
	}
	if cfg.PublicPort <= 0 {
		return nil, fmt.Errorf("local ingress public port must be greater than 0")
	}
	if cfg.AdminPort <= 0 {
		cfg.AdminPort = defaultLocalIngressAdminPort
	}
	if cfg.Image == "" {
		cfg.Image = defaultLocalIngressImage
	}
	if err := EnsureDockerNetwork(ctx, runner, cfg.DockerNetwork); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir local ingress state dir: %w", err)
	}
	configPath, err := writeLocalIngressBootstrapConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := removeContainer(context.Background(), runner, cfg.ContainerName); err != nil {
		return nil, fmt.Errorf("remove existing local ingress container %s: %w", cfg.ContainerName, err)
	}
	args := []string{
		"run", "--detach", "--rm",
		"--name", cfg.ContainerName,
		"--network", cfg.DockerNetwork,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.PublicPort, cfg.PublicPort),
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.AdminPort, defaultLocalIngressAdminPort),
		"--volume", configPath + ":/etc/caddy/local.json:ro",
		cfg.Image,
		"caddy", "run", "--config", "/etc/caddy/local.json",
	}
	if _, err := runner.Run(ctx, args...); err != nil {
		return nil, err
	}
	managed := &ManagedIngress{cfg: cfg, runner: runner}
	if err := managed.waitUntilReady(ctx); err != nil {
		_ = managed.Close()
		return nil, err
	}
	return managed, nil
}

func (m *ManagedIngress) AdminURL() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/load", m.cfg.AdminPort)
}

func (m *ManagedIngress) BaseURL() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", m.cfg.PublicHost, m.cfg.PublicPort)
}

func (m *ManagedIngress) Close() error {
	if m == nil {
		return nil
	}
	return removeContainer(context.Background(), m.runner, m.cfg.ContainerName)
}

func (m *ManagedIngress) waitUntilReady(ctx context.Context) error {
	adminConfigURL := fmt.Sprintf("http://127.0.0.1:%d/config/", m.cfg.AdminPort)
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminConfigURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode < 300, nil
	})
}

func writeLocalIngressBootstrapConfig(cfg LocalIngressConfig) (string, error) {
	path := filepath.Join(cfg.StateDir, "caddy-bootstrap.json")
	payload := map[string]any{
		"admin": map[string]any{
			"listen": fmt.Sprintf(":%d", defaultLocalIngressAdminPort),
		},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen": []string{fmt.Sprintf(":%d", cfg.PublicPort)},
						"routes": []any{
							map[string]any{
								"handle": []any{
									map[string]any{
										"handler":     "static_response",
										"status_code": 503,
										"body":        "local ingress not ready",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal local ingress bootstrap config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("write local ingress bootstrap config: %w", err)
	}
	return path, nil
}

func removeContainer(ctx context.Context, runner DockerRunner, name string) error {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	_, err := runner.Run(ctx, "rm", "--force", name)
	if isDockerMissingObjectError(err) {
		return nil
	}
	return err
}
