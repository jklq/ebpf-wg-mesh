package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/testutil"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

type stackSummary struct {
	ControlPlaneURL string `json:"control_plane_url"`
	DashboardURL    string `json:"dashboard_url"`
	DatabaseURL     string `json:"database_url"`
	ArtifactsDir    string `json:"artifacts_dir"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	repoRoot, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}
	dashboardDir := filepath.Join(repoRoot, "dashboard")
	artifactsDir := filepath.Join(dashboardDir, "artifacts", "e2e-local")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		log.Fatalf("mkdir artifacts: %v", err)
	}

	cockroach, err := testserver.NewTestServer(testserver.CustomVersionOpt("v26.1.0"))
	if err != nil {
		log.Fatalf("start cockroach testserver: %v", err)
	}
	defer cockroach.Stop()

	dbURL := normalizeURL(cockroach.PGURL()).String()
	stateDir := filepath.Join(os.TempDir(), fmt.Sprintf("ebpf-wg-mesh-localtest-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Fatalf("mkdir state dir: %v", err)
	}
	defer os.RemoveAll(stateDir)

	cfg := config.ControlPlaneConfig{
		PublicHTTP: config.ListenerConfig{
			Listen: "127.0.0.1:0",
		},
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"controlplane", "localhost"},
				BootstrapTokens:         []string{"agent-bootstrap-token"},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: config.DatabaseConfig{
			URL: dbURL,
		},
		StateDir: stateDir,
		Ingress: config.IngressConfig{
			AdminURL:                 "http://127.0.0.1:2019/load",
			PublicAddr:               "platform.local",
			ControlPlaneHTTPUpstream: "127.0.0.1:8080",
		},
		Bootstrap: config.BootstrapConfig{
			Users: []config.BootstrapUser{{
				Subject: "dev-user",
				Email:   "dev@example.com",
			}},
		},
		Mesh: config.ControlPlaneMeshConfig{
			InterfaceName:              "wg0",
			ListenPort:                 51820,
			NetworkCIDR:                "fd00:44::/64",
			WorkloadPoolCIDR:           "fd00:200::/48",
			PersistentKeepaliveSeconds: 5,
		},
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		log.Fatalf("finalize controlplane config: %v", err)
	}

	server, err := controlplane.NewServer(ctx, cfg)
	if err != nil {
		log.Fatalf("create controlplane server: %v", err)
	}
	defer server.Close()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- server.Run(ctx)
	}()

	controlPlaneURL := "http://" + server.PublicAddr()
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, controlPlaneURL+"/healthz", nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}); err != nil {
		log.Fatalf("wait for controlplane health: %v", err)
	}

	identity, err := server.EnsureDashboardClientIdentity("dashboard-local")
	if err != nil {
		log.Fatalf("mint dashboard client identity: %v", err)
	}

	dashboardURL, dashboardCmd, err := startDashboard(ctx, dashboardDir, map[string]string{
		"DASHBOARD_DATABASE_URL":              dbURL,
		"DASHBOARD_DATABASE_SCHEMA":           "dashboard_local_e2e",
		"DASHBOARD_SESSION_COOKIE_NAME":       "dashboard_local_e2e_session",
		"DASHBOARD_PUBLIC_BASE_URL":           "http://127.0.0.1",
		"DASHBOARD_CONTROLPLANE_ADDRESS":      server.InternalAddr(),
		"DASHBOARD_CONTROLPLANE_SERVER_NAME":  "localhost",
		"DASHBOARD_CONTROLPLANE_CA_PEM_B64":   base64.StdEncoding.EncodeToString(identity.CAPEM),
		"DASHBOARD_CONTROLPLANE_CERT_PEM_B64": base64.StdEncoding.EncodeToString(identity.CertPEM),
		"DASHBOARD_CONTROLPLANE_KEY_PEM_B64":  base64.StdEncoding.EncodeToString(identity.KeyPEM),
		"DASHBOARD_DEV_USERS":                 "dev-user:dev@example.com",
	})
	if err != nil {
		log.Fatalf("start dashboard: %v", err)
	}
	defer stopProcess(dashboardCmd)

	summary := stackSummary{
		ControlPlaneURL: controlPlaneURL,
		DashboardURL:    dashboardURL,
		DatabaseURL:     dbURL,
		ArtifactsDir:    artifactsDir,
	}
	if err := writeSummary(filepath.Join(artifactsDir, "stack.json"), summary); err != nil {
		log.Fatalf("write stack summary: %v", err)
	}

	runPlaywright := os.Getenv("LOCALTESTSTACK_RUN_PLAYWRIGHT") != "0"
	if runPlaywright {
		playwright := exec.CommandContext(ctx, "bun", "run", "test:e2e:local")
		playwright.Dir = dashboardDir
		playwright.Stdout = os.Stdout
		playwright.Stderr = os.Stderr
		playwright.Env = append(os.Environ(),
			"DASHBOARD_E2E_BASE_URL="+dashboardURL,
		)
		if err := playwright.Run(); err != nil {
			log.Fatalf("run playwright: %v", err)
		}
	} else {
		log.Printf("ephemeral stack ready")
		log.Printf("dashboard: %s", dashboardURL)
		log.Printf("controlplane: %s", controlPlaneURL)
		log.Printf("database: %s", dbURL)
		log.Printf("artifacts: %s", artifactsDir)
		<-ctx.Done()
	}

	select {
	case err := <-runErrCh:
		if err != nil {
			log.Fatalf("controlplane exited: %v", err)
		}
	default:
	}
}

func startDashboard(ctx context.Context, dashboardDir string, env map[string]string) (string, *exec.Cmd, error) {
	dashboardURL := "http://127.0.0.1:3000/"

	cmd := exec.CommandContext(ctx, "bun", "run", "dev")
	cmd.Dir = dashboardDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start dashboard process: %w", err)
	}

	healthURL := dashboardURL + "healthz"
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}); err != nil {
		stopProcess(cmd)
		return "", nil, fmt.Errorf("wait for dashboard health at %s: %w", healthURL, err)
	}

	return dashboardURL, cmd, nil
}

func writeSummary(path string, summary stackSummary) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_, _ = cmd.Process.Wait()
}

func normalizeURL(source *url.URL) *url.URL {
	if source == nil {
		log.Fatal("nil cockroach pg url")
	}
	clone := *source
	query := clone.Query()
	if strings.TrimSpace(query.Get("sslmode")) == "" {
		query.Set("sslmode", "disable")
	}
	clone.RawQuery = query.Encode()
	return &clone
}
