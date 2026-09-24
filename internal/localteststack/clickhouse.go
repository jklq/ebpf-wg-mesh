package localteststack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/testutil"
)

const (
	defaultLocalClickHouseImage      = "clickhouse/clickhouse-server:25.3"
	defaultLocalClickHouseNativePort = 9000
)

type LocalClickHouseConfig struct {
	ContainerName string
	NativePort    int
	Image         string
}

type ManagedClickHouse struct {
	cfg    LocalClickHouseConfig
	runner DockerRunner
}

func StartManagedClickHouse(ctx context.Context, cfg LocalClickHouseConfig, runner DockerRunner) (*ManagedClickHouse, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if strings.TrimSpace(cfg.ContainerName) == "" {
		return nil, fmt.Errorf("local clickhouse container name is required")
	}
	if cfg.NativePort <= 0 {
		cfg.NativePort = defaultLocalClickHouseNativePort
	}
	if cfg.Image == "" {
		cfg.Image = defaultLocalClickHouseImage
	}
	if err := removeContainer(context.Background(), runner, cfg.ContainerName); err != nil {
		return nil, fmt.Errorf("remove existing local clickhouse container %s: %w", cfg.ContainerName, err)
	}
	// No --rm: tests simulate backend outages by stopping and restarting
	// this container; Close removes it explicitly.
	args := []string{
		"run", "--detach",
		"--name", cfg.ContainerName,
		"--memory", "768m", "--memory-swap", "768m",
		"--ulimit", "nofile=262144:262144",
		"--env", "CLICKHOUSE_SKIP_USER_SETUP=1",
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.NativePort, defaultLocalClickHouseNativePort),
		cfg.Image,
	}
	if _, err := runner.Run(ctx, args...); err != nil {
		return nil, err
	}
	managed := &ManagedClickHouse{cfg: cfg, runner: runner}
	if err := managed.waitUntilReady(ctx); err != nil {
		_ = managed.Close()
		return nil, err
	}
	return managed, nil
}

// Stop halts the server without removing the container, simulating a
// backend outage. Start brings the same container back.
func (m *ManagedClickHouse) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	_, err := m.runner.Run(ctx, "stop", m.cfg.ContainerName)
	return err
}

func (m *ManagedClickHouse) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	_, err := m.runner.Run(ctx, "start", m.cfg.ContainerName)
	return err
}

func (m *ManagedClickHouse) URL() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("clickhouse://127.0.0.1:%d/default", m.cfg.NativePort)
}

func (m *ManagedClickHouse) Close() error {
	if m == nil {
		return nil
	}
	return removeContainer(context.Background(), m.runner, m.cfg.ContainerName)
}

func (m *ManagedClickHouse) waitUntilReady(ctx context.Context) error {
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 60 * time.Second}, func(ctx context.Context) (bool, error) {
		_, err := m.runner.Run(ctx, "exec", m.cfg.ContainerName, "clickhouse-client", "--query", "SELECT 1")
		return err == nil, nil
	})
}
