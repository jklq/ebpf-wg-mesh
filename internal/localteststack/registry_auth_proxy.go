package localteststack

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/testutil"
)

const defaultRegistryAuthProxyImage = "alpine/socat:1.8.0.0"

type LocalRegistryAuthProxyConfig struct {
	ContainerName string
	UpstreamHost  string
	UpstreamPort  int
	HostPort      int
	Image         string
}

type ManagedRegistryAuthProxy struct {
	cfg    LocalRegistryAuthProxyConfig
	runner DockerRunner
}

func StartManagedRegistryAuthProxy(ctx context.Context, cfg LocalRegistryAuthProxyConfig, runner DockerRunner) (*ManagedRegistryAuthProxy, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if strings.TrimSpace(cfg.ContainerName) == "" {
		return nil, fmt.Errorf("registry auth proxy container name is required")
	}
	if strings.TrimSpace(cfg.UpstreamHost) == "" {
		return nil, fmt.Errorf("registry auth proxy upstream host is required")
	}
	if cfg.UpstreamPort <= 0 || cfg.UpstreamPort > 65535 {
		return nil, fmt.Errorf("registry auth proxy upstream port is invalid")
	}
	if cfg.HostPort <= 0 || cfg.HostPort > 65535 {
		return nil, fmt.Errorf("registry auth proxy host port is invalid")
	}
	if cfg.Image == "" {
		cfg.Image = defaultRegistryAuthProxyImage
	}

	if err := removeContainer(context.Background(), runner, cfg.ContainerName); err != nil {
		return nil, fmt.Errorf("remove existing registry auth proxy container %s: %w", cfg.ContainerName, err)
	}
	listenPort := cfg.HostPort
	args := []string{
		"run", "--detach", "--rm",
		"--name", cfg.ContainerName,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HostPort, listenPort),
		cfg.Image,
		fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", listenPort),
		fmt.Sprintf("TCP:%s:%d", cfg.UpstreamHost, cfg.UpstreamPort),
	}
	if _, err := runner.Run(ctx, args...); err != nil {
		return nil, err
	}
	managed := &ManagedRegistryAuthProxy{cfg: cfg, runner: runner}
	if err := managed.waitUntilReady(ctx); err != nil {
		_ = managed.Close()
		return nil, err
	}
	return managed, nil
}

func (m *ManagedRegistryAuthProxy) TokenRealmBaseURL() string {
	if m == nil {
		return ""
	}
	return "http://localhost:" + strconv.Itoa(m.cfg.HostPort)
}

func (m *ManagedRegistryAuthProxy) Close() error {
	if m == nil {
		return nil
	}
	return removeContainer(context.Background(), m.runner, m.cfg.ContainerName)
}

func (m *ManagedRegistryAuthProxy) waitUntilReady(ctx context.Context) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(m.cfg.HostPort))
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second, Interval: 100 * time.Millisecond}, func(context.Context) (bool, error) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	})
}
