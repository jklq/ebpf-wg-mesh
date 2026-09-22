package localteststack

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/testutil"
)

const (
	defaultLocalIngressImage     = "envoyproxy/envoy:v1.36-latest"
	defaultLocalIngressAdminPort = 19000
	envoyContainerAdminPort      = 19000
)

type LocalIngressConfig struct {
	StateDir      string
	DockerNetwork string
	ContainerName string
	NodeID        string
	XDSServerAddr string
	PublicHost    string
	PublicPort    int
	AdminPort     int
	Image         string
}

type ManagedIngress struct {
	cfg    LocalIngressConfig
	runner DockerRunner
}

// StartManagedIngress runs the Envoy container. Call WaitReady after the xDS
// control plane is serving: Envoy's /ready stays 503 until it has pulled
// LDS/CDS/RDS over ADS.
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
	if strings.TrimSpace(cfg.NodeID) == "" {
		return nil, fmt.Errorf("local ingress node id is required")
	}
	if strings.TrimSpace(cfg.XDSServerAddr) == "" {
		return nil, fmt.Errorf("local ingress xds server address is required")
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
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.AdminPort, envoyContainerAdminPort),
		"--volume", configPath + ":/etc/envoy/envoy.yaml:ro",
		cfg.Image,
		"envoy", "--config-path", "/etc/envoy/envoy.yaml",
	}
	if _, err := runner.Run(ctx, args...); err != nil {
		return nil, err
	}
	return &ManagedIngress{cfg: cfg, runner: runner}, nil
}

func (m *ManagedIngress) AdminURL() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", m.cfg.AdminPort)
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

// WaitReady blocks until Envoy's admin /ready returns 2xx. On timeout the
// container logs are included in the error.
func (m *ManagedIngress) WaitReady(ctx context.Context) error {
	readyURL := fmt.Sprintf("http://127.0.0.1:%d/ready", m.cfg.AdminPort)
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyURL, nil)
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
	if err != nil {
		logs, _ := m.runner.Run(ctx, "logs", "--tail", "100", m.cfg.ContainerName)
		return fmt.Errorf("%w\nenvoy container logs:\n%s", err, logs)
	}
	return nil
}

func writeLocalIngressBootstrapConfig(cfg LocalIngressConfig) (string, error) {
	bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{
		NodeID:       cfg.NodeID,
		XDSAddresses: []string{cfg.XDSServerAddr},
		AdminAddress: fmt.Sprintf("0.0.0.0:%d", envoyContainerAdminPort),
	})
	if err != nil {
		return "", err
	}
	path := filepath.Join(cfg.StateDir, "envoy-bootstrap.yaml")
	if err := os.WriteFile(path, []byte(bootstrap), 0o644); err != nil {
		return "", fmt.Errorf("write local ingress bootstrap config: %w", err)
	}
	return path, nil
}
