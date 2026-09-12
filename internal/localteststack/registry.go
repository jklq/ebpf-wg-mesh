package localteststack

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/testutil"
)

const (
	defaultLocalRegistryImage = "registry:2"
	localRegistryPort         = 5000
)

type LocalRegistryConfig struct {
	StateDir       string
	ContainerName  string
	HostPort       int
	TokenRealm     string
	TokenService   string
	TokenIssuer    string
	RootCertBundle string
	Image          string
}

type ManagedRegistry struct {
	cfg    LocalRegistryConfig
	runner DockerRunner
}

func StartManagedRegistry(ctx context.Context, cfg LocalRegistryConfig, runner DockerRunner) (*ManagedRegistry, error) {
	if runner == nil {
		runner = ExecDockerRunner{}
	}
	if strings.TrimSpace(cfg.StateDir) == "" || strings.TrimSpace(cfg.ContainerName) == "" {
		return nil, fmt.Errorf("local registry state dir and container name are required")
	}
	if cfg.HostPort <= 0 || cfg.TokenRealm == "" || cfg.TokenService == "" || cfg.TokenIssuer == "" || cfg.RootCertBundle == "" {
		return nil, fmt.Errorf("local registry port and token auth configuration are required")
	}
	if cfg.Image == "" {
		cfg.Image = defaultLocalRegistryImage
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "data"), 0o755); err != nil {
		return nil, fmt.Errorf("create local registry state: %w", err)
	}
	configPath := filepath.Join(cfg.StateDir, "config.yml")
	configBody := fmt.Sprintf(`version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: :5000
auth:
  token:
    realm: %q
    service: %q
    issuer: %q
    rootcertbundle: /auth/signing-cert.pem
`, cfg.TokenRealm, cfg.TokenService, cfg.TokenIssuer)
	if err := os.WriteFile(configPath, []byte(configBody), 0o644); err != nil {
		return nil, fmt.Errorf("write local registry config: %w", err)
	}
	if err := removeContainer(context.Background(), runner, cfg.ContainerName); err != nil {
		return nil, fmt.Errorf("remove existing local registry container %s: %w", cfg.ContainerName, err)
	}
	args := []string{
		"run", "--detach", "--rm",
		"--name", cfg.ContainerName,
	}
	// Run as the invoking user so blobs written to the bind-mounted data dir stay
	// owned by the test process. Otherwise the registry runs as root and its
	// root-owned files break TempDir cleanup for non-root users on Linux.
	if uid, gid := os.Getuid(), os.Getgid(); uid >= 0 && gid >= 0 {
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, gid))
	}
	args = append(args,
		"--publish", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HostPort, localRegistryPort),
		"--volume", configPath+":/etc/docker/registry/config.yml:ro",
		"--volume", filepath.Join(cfg.StateDir, "data")+":/var/lib/registry",
		"--volume", cfg.RootCertBundle+":/auth/signing-cert.pem:ro",
		cfg.Image,
	)
	if _, err := runner.Run(ctx, args...); err != nil {
		return nil, err
	}
	managed := &ManagedRegistry{cfg: cfg, runner: runner}
	if err := managed.waitUntilReady(ctx); err != nil {
		_ = managed.Close()
		return nil, err
	}
	return managed, nil
}

func (m *ManagedRegistry) Host() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("localhost:%d", m.cfg.HostPort)
}

func (m *ManagedRegistry) Close() error {
	if m == nil {
		return nil
	}
	return removeContainer(context.Background(), m.runner, m.cfg.ContainerName)
}

func (m *ManagedRegistry) waitUntilReady(ctx context.Context) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", m.cfg.HostPort)
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 60 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized && strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer"), nil
	})
}
