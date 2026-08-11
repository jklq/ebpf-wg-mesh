package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/localteststack"
	"ebof-wg-mesh/internal/testutil"

	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

type stackSummary struct {
	ControlPlaneURL   string `json:"control_plane_url"`
	DashboardURL      string `json:"dashboard_url"`
	DatabaseURL       string `json:"database_url"`
	ClickHouseURL     string `json:"clickhouse_url"`
	ArtifactsDir      string `json:"artifacts_dir"`
	PublicBaseURL     string `json:"public_base_url"`
	GitHubEnabled     bool   `json:"github_enabled"`
	GitHubCallbackURL string `json:"github_callback_url"`
	GitHubWebhookURL  string `json:"github_webhook_url"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	repoRoot, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}
	// Optional local overrides, including a 1Password Environments-mounted .env FIFO.
	// Existing process env wins; missing .env is fine.
	if n, err := localteststack.LoadDotEnvFile(filepath.Join(repoRoot, ".env")); err != nil {
		log.Fatalf("load .env: %v", err)
	} else if n > 0 {
		log.Printf("loaded %d variable(s) from .env", n)
	}
	stackCfg, err := loadLocalStackConfig(os.Getenv)
	if err != nil {
		log.Fatalf("load localteststack config: %v", err)
	}
	consoleDir := filepath.Join(repoRoot, "console")
	artifactsDir := filepath.Join(consoleDir, "artifacts", "e2e-local")
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
	consolePort, err := pickConsolePort()
	if err != nil {
		log.Fatalf("pick console port: %v", err)
	}
	ingressAdminPort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick ingress admin port: %v", err)
	}
	clickHousePort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick clickhouse port: %v", err)
	}
	ingress, err := localteststack.StartManagedIngress(ctx, localteststack.LocalIngressConfig{
		StateDir:      filepath.Join(stateDir, "local-ingress"),
		DockerNetwork: stackCfg.DockerNetwork,
		ContainerName: "localteststack-caddy",
		PublicHost:    stackCfg.IngressHost,
		PublicPort:    stackCfg.IngressPort,
		AdminPort:     ingressAdminPort,
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		log.Fatalf("start managed local ingress: %v", err)
	}
	defer func() {
		if err := ingress.Close(); err != nil {
			log.Printf("stop managed local ingress: %v", err)
		}
		if err := localteststack.RemoveDockerNetwork(context.Background(), localteststack.ExecDockerRunner{}, stackCfg.DockerNetwork); err != nil {
			log.Printf("remove local docker network: %v", err)
		}
	}()
	ingressURL := ingress.BaseURL() + "/"
	clickHouse, err := localteststack.StartManagedClickHouse(ctx, localteststack.LocalClickHouseConfig{
		ContainerName: "localteststack-clickhouse",
		NativePort:    clickHousePort,
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		log.Fatalf("start managed local clickhouse: %v", err)
	}
	defer func() {
		if err := clickHouse.Close(); err != nil {
			log.Printf("stop managed local clickhouse: %v", err)
		}
	}()
	clickHouseURL := clickHouse.URL()

	cfg := config.ControlPlaneConfig{
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames:             []string{"controlplane", "localhost"},
				BootstrapTokens:         []config.AgentBootstrapToken{{AgentID: localAgentID, Token: "agent-bootstrap-token"}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: config.DatabaseConfig{
			URL: dbURL,
		},
		Logs: config.LogCaptureConfig{
			ClickHouse: config.ClickHouseConfig{
				URL: clickHouseURL,
			},
		},
		StateDir: stateDir,
		Ingress: config.IngressConfig{
			AdminURL:              ingress.AdminURL(),
			AdminListen:           ":2019",
			ListenAddrs:           []string{fmt.Sprintf(":%d", stackCfg.IngressPort)},
			DisableAutomaticHTTPS: true,
			PublicAddr:            stackCfg.IngressHost,
			StaticRoutes: []config.StaticIngressRouteConfig{{
				Hosts:    []string{stackCfg.IngressHost},
				Upstream: fmt.Sprintf("host.docker.internal:%d", consolePort),
			}},
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
	consoleEnv := map[string]string{
		"DASHBOARD_DATABASE_URL":           dbURL,
		"DASHBOARD_DATABASE_SCHEMA":        "dashboard_local_e2e",
		"DASHBOARD_SESSION_COOKIE_NAME":    "dashboard_local_e2e_session",
		"DASHBOARD_JWT_SECRET":             "dashboard-local-e2e-jwt-secret",
		"DASHBOARD_PUBLIC_BASE_URL":        ingressURL[:len(ingressURL)-1],
		"DASHBOARD_LOCAL_INGRESS_BASE_URL": ingressURL[:len(ingressURL)-1],
		"DASHBOARD_INGRESS_TARGET_HOST":    stackCfg.IngressHost,
		"DASHBOARD_LOCAL_DOMAIN_SUFFIX":    stackCfg.LocalDomainSuffix,
		"DASHBOARD_DEV_USERS":              "dev-user:dev@example.com",
	}

	overlay := localteststack.OverlayResult{}
	// Prefer secrets already in process env (shell exports and/or mounted .env).
	// The SDK path is only needed for headless/automation when OP_* is configured
	// and the local contract is still incomplete.
	processOverlay := localteststack.OverlayEnvFromLookup(os.Getenv)
	overlayEnv := processOverlay
	loader := localteststack.NewEnvironmentLoader(localteststack.EnvironmentLoaderConfigFromLookup(os.Getenv))
	publicBaseURL := ""
	processComplete := len(localteststack.MissingGitHubKeys(processOverlay)) == 0
	switch {
	case processComplete && loader.Enabled():
		log.Printf(
			"using GitHub devstack secrets from process environment/.env; skipping 1Password SDK (%s is set but not required)",
			localteststack.OPEnvironmentIDKey,
		)
	case processComplete:
		log.Printf("using GitHub devstack secrets from process environment/.env")
	case !loader.Enabled():
		if len(processOverlay) > 0 {
			log.Printf(
				"GitHub devstack secrets from process environment/.env are incomplete; missing keys: %s; GitHub mode stays disabled",
				strings.Join(localteststack.MissingGitHubKeys(processOverlay), ", "),
			)
		}
	case !loader.Ready():
		log.Printf(
			"1Password environment loading skipped: %s is set but neither %s nor %s is configured; set one of those, mount a 1Password local .env, or export the required env vars directly",
			localteststack.OPEnvironmentIDKey,
			localteststack.OPServiceAccountTokenKey,
			localteststack.OPAccountKey,
		)
	default:
		log.Printf("loading 1Password environment via SDK")
		loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		env, err := loader.Load(loadCtx)
		cancel()
		if err != nil {
			log.Fatalf("%s", describeOnePasswordLoadError(err))
		}
		// Process/shell/.env values win over remote SDK values.
		overlayEnv = localteststack.MergeOverlayEnv(env, processOverlay)
		log.Printf("loaded 1Password environment via SDK")
		missingGitHubKeys := localteststack.MissingGitHubKeys(overlayEnv)
		if len(missingGitHubKeys) > 0 {
			log.Printf(
				"1Password environment is incomplete for GitHub devstack enablement; missing keys: %s; GitHub mode stays disabled until these are present",
				strings.Join(missingGitHubKeys, ", "),
			)
		}
	}
	cloudflareTunnelToken := strings.TrimSpace(overlayEnv[localteststack.CloudflareTunnelTokenKey])
	if cloudflareTunnelToken == "" {
		cloudflareTunnelToken = strings.TrimSpace(os.Getenv(localteststack.CloudflareTunnelTokenKey))
	}
	cloudflareHostname := strings.TrimSpace(overlayEnv[localteststack.CloudflareHostnameKey])
	if cloudflareHostname == "" {
		cloudflareHostname = strings.TrimSpace(os.Getenv(localteststack.CloudflareHostnameKey))
	}
	if cloudflareTunnelRequested(overlayEnv, cloudflareTunnelToken, cloudflareHostname) {
		if missingRuntimeKeys := missingCloudflareRuntimeKeys(cloudflareTunnelToken, cloudflareHostname); len(missingRuntimeKeys) > 0 {
			log.Fatalf(
				"Cloudflare tunnel configuration is incomplete; set these env vars before starting again: %s. Cloudflare should already route %s to the local ingress origin %s",
				strings.Join(missingRuntimeKeys, ", "),
				cloudflareHostname,
				ingressURL[:len(ingressURL)-1],
			)
		}
		log.Printf(
			"starting cloudflare tunnel for %s; Cloudflare should already route this hostname to the local ingress origin %s",
			cloudflareHostname,
			ingressURL[:len(ingressURL)-1],
		)
		publicURL, err := startCloudflareTunnel(ctx, cloudflareTunnelToken, cloudflareHostname)
		if err != nil {
			if startupInterrupted(ctx) {
				return
			}
			log.Fatalf("%s", describeCloudflareStartupError(err, cloudflareHostname))
		}
		log.Printf("cloudflare tunnel ready: %s", publicURL.BaseURL)
		publicBaseURL = publicURL.BaseURL
		if publicURL.Close != nil {
			defer func() {
				if err := publicURL.Close(); err != nil {
					log.Printf("close cloudflare tunnel: %v", err)
				}
			}()
		}
	}
	overlay, err = localteststack.ApplyEnvironmentOverlay(&cfg, consoleEnv, overlayEnv, publicBaseURL)
	if err != nil {
		log.Fatalf("apply 1Password environment overlay: %v", err)
	}
	if parsedPublicURL, err := url.Parse(overlay.PublicBaseURL); err == nil && parsedPublicURL.Host != "" && len(cfg.Ingress.StaticRoutes) > 0 {
		cfg.Ingress.StaticRoutes[0].Hosts = appendUniqueStrings(cfg.Ingress.StaticRoutes[0].Hosts, parsedPublicURL.Host)
	}
	if overlay.PublicBaseURL != "" {
		log.Printf("public base url: %s", overlay.PublicBaseURL)
		log.Printf("github enabled: %t", overlay.GitHubEnabled)
		log.Printf("github callback url: %s", overlay.GitHubCallbackURL)
		log.Printf("github webhook url: %s", overlay.GitHubWebhookURL)
	}
	if startupInterrupted(ctx) {
		return
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

	controlPlaneURL := server.InternalAddr()
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second}, func(ctx context.Context) (bool, error) {
		conn, err := net.DialTimeout("tcp", controlPlaneURL, 200*time.Millisecond)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	}); err != nil {
		log.Fatalf("wait for controlplane grpc listener: %v", err)
	}

	identity, err := server.EnsureDashboardClientIdentity("dashboard-local")
	if err != nil {
		log.Fatalf("mint dashboard client identity: %v", err)
	}

	localAgent, localAgentErrCh, err := startLocalAgent(ctx, stackCfg, stateDir, controlPlaneURL, identity.CAPEM)
	if err != nil {
		log.Fatalf("start local agent: %v", err)
	}
	defer localAgent.Close()
	if err := waitForLocalAgent(ctx, server, localAgentID, localAgentErrCh); err != nil {
		log.Fatalf("wait for local agent: %v", err)
	}
	log.Printf("local agent ready: %s", localAgentID)
	go monitorBackgroundComponent(ctx, stop, "local agent", localAgentErrCh)

	if overlay.GitHubEnabled {
		localBuilder, localBuilderErrCh, err := startLocalBuilder(ctx, stateDir, controlPlaneURL, server)
		if err != nil {
			log.Fatalf("start local builder: %v", err)
		}
		defer localBuilder.Close()
		log.Printf("local builder ready: %s", localBuilderID)
		go monitorBackgroundComponent(ctx, stop, "local builder", localBuilderErrCh)
	}

	consoleEnv["DASHBOARD_CONTROLPLANE_ADDRESS"] = server.InternalAddr()
	consoleEnv["DASHBOARD_CONTROLPLANE_SERVER_NAME"] = "localhost"
	consoleEnv["DASHBOARD_CONTROLPLANE_CA_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.CAPEM)
	consoleEnv["DASHBOARD_CONTROLPLANE_CERT_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.CertPEM)
	consoleEnv["DASHBOARD_CONTROLPLANE_KEY_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.KeyPEM)

	_, consoleCmd, err := startConsole(ctx, consoleDir, consoleEnv, consolePort)
	if err != nil {
		log.Fatalf("start console: %v", err)
	}
	defer stopProcess(consoleCmd)
	if err := waitForIngressDashboard(ctx, ingressURL+"healthz"); err != nil {
		log.Fatalf("wait for ingress dashboard health: %v", err)
	}

	summary := stackSummary{
		ControlPlaneURL:   controlPlaneURL,
		DashboardURL:      ingressURL,
		DatabaseURL:       dbURL,
		ClickHouseURL:     clickHouseURL,
		ArtifactsDir:      artifactsDir,
		PublicBaseURL:     firstNonEmpty(overlay.PublicBaseURL, ingressURL[:len(ingressURL)-1]),
		GitHubEnabled:     overlay.GitHubEnabled,
		GitHubCallbackURL: overlay.GitHubCallbackURL,
		GitHubWebhookURL:  overlay.GitHubWebhookURL,
	}
	if err := writeSummary(filepath.Join(artifactsDir, "stack.json"), summary); err != nil {
		log.Fatalf("write stack summary: %v", err)
	}

	runPlaywright := os.Getenv("LOCALTESTSTACK_RUN_PLAYWRIGHT") != "0"
	if runPlaywright {
		playwright := exec.CommandContext(ctx, "bun", "run", "test:e2e:local")
		playwright.Dir = consoleDir
		playwright.Stdout = os.Stdout
		playwright.Stderr = os.Stderr
		playwright.Env = append(os.Environ(),
			"DASHBOARD_E2E_BASE_URL="+ingressURL[:len(ingressURL)-1],
		)
		if err := playwright.Run(); err != nil {
			log.Fatalf("run playwright: %v", err)
		}
	} else {
		log.Printf("ephemeral stack ready")
		log.Printf("dashboard: %s", ingressURL)
		log.Printf("example published host: http://%s:%d", stackCfg.examplePublishedHost("echo"), stackCfg.IngressPort)
		log.Printf("controlplane grpc: %s", controlPlaneURL)
		log.Printf("database: %s", dbURL)
		log.Printf("clickhouse: %s", clickHouseURL)
		log.Printf("artifacts: %s", artifactsDir)
		if overlay.PublicBaseURL != "" {
			log.Printf("public base url: %s", overlay.PublicBaseURL)
			log.Printf("github enabled: %t", overlay.GitHubEnabled)
			log.Printf("github callback url: %s", overlay.GitHubCallbackURL)
			log.Printf("github webhook url: %s", overlay.GitHubWebhookURL)
		}
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

func startConsole(ctx context.Context, consoleDir string, env map[string]string, port int) (string, *exec.Cmd, error) {
	consoleURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	cmd := exec.CommandContext(ctx, "bun", "--bun", "vite", "dev", "--host", "0.0.0.0", "--port", strconv.Itoa(port), "--strictPort")
	cmd.Dir = consoleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start console process: %w", err)
	}

	healthURL := consoleURL + "healthz"
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
		return "", nil, fmt.Errorf("wait for console health at %s: %w", healthURL, err)
	}

	return consoleURL, cmd, nil
}

func pickConsolePort() (int, error) {
	return pickLoopbackPort()
}

func pickLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return addr.Port, nil
}

func waitForIngressDashboard(ctx context.Context, healthURL string) error {
	return testutil.Poll(ctx, testutil.PollConfig{Timeout: 30 * time.Second}, func(ctx context.Context) (bool, error) {
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
	})
}

func appendUniqueStrings(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return items
		}
	}
	return append(items, value)
}

func cloudflareTunnelRequested(env map[string]string, token, hostname string) bool {
	return len(localteststack.MissingGitHubKeys(env)) == 0 ||
		strings.TrimSpace(token) != "" ||
		strings.TrimSpace(hostname) != ""
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

func startCloudflareTunnel(ctx context.Context, tunnelToken string, hostname string) (localteststack.PublicURLResult, error) {
	tunnelCtx, cancelTunnel := context.WithCancel(ctx)

	type result struct {
		publicURL localteststack.PublicURLResult
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		publicURL, err := localteststack.StartCloudflareTunnel(tunnelCtx, tunnelToken, hostname)
		resultCh <- result{publicURL: publicURL, err: err}
	}()

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		cancelTunnel()
		return localteststack.PublicURLResult{}, ctx.Err()
	case <-timer.C:
		cancelTunnel()
		return localteststack.PublicURLResult{}, context.DeadlineExceeded
	case result := <-resultCh:
		if result.err != nil {
			cancelTunnel()
			return localteststack.PublicURLResult{}, result.err
		}
		closeFn := result.publicURL.Close
		result.publicURL.Close = func() error {
			cancelTunnel()
			if closeFn != nil {
				return closeFn()
			}
			return nil
		}
		return result.publicURL, nil
	}
}

func missingCloudflareRuntimeKeys(tunnelToken string, hostname string) []string {
	missing := make([]string, 0, 2)
	if strings.TrimSpace(tunnelToken) == "" {
		missing = append(missing, localteststack.CloudflareTunnelTokenKey)
	}
	if strings.TrimSpace(hostname) == "" {
		missing = append(missing, localteststack.CloudflareHostnameKey)
	}
	return missing
}

func describeOnePasswordLoadError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "load 1Password environment: timed out after 30s; use OP_SERVICE_ACCOUNT_TOKEN or fix desktop-app integration"
	case errors.Is(err, context.Canceled):
		return "load 1Password environment: startup interrupted"
	}

	message := err.Error()
	if strings.Contains(message, "Account not found") {
		return fmt.Sprintf(
			"load 1Password environment: desktop-app auth could not find %s; set %s to the exact account UUID/name shown by 1Password, or use %s instead",
			localteststack.OPAccountKey,
			localteststack.OPAccountKey,
			localteststack.OPServiceAccountTokenKey,
		)
	}
	if strings.Contains(message, "invalid service account token") ||
		strings.Contains(message, "base64 decoding failed") ||
		strings.Contains(message, "service account token") {
		return fmt.Sprintf(
			"load 1Password environment: invalid %s (SDK could not decode it). For local dev, prefer a 1Password Environments-mounted repo-root .env with the GitHub/devstack keys and unset %s/%s; for headless SDK use, set %s to a full token starting with ops_ from a 1Password service account",
			localteststack.OPServiceAccountTokenKey,
			localteststack.OPEnvironmentIDKey,
			localteststack.OPServiceAccountTokenKey,
			localteststack.OPServiceAccountTokenKey,
		)
	}
	return fmt.Sprintf("load 1Password environment: %v", err)
}

func describeCloudflareStartupError(err error, hostname string) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("start cloudflare tunnel for %s: timed out after 30s; verify `cloudflared` is installed, then confirm %s points at a pre-provisioned Cloudflare Tunnel", hostname, localteststack.CloudflareHostnameKey)
	case errors.Is(err, context.Canceled):
		return "start cloudflare tunnel: startup interrupted"
	}

	message := err.Error()
	switch {
	case strings.Contains(message, localteststack.CloudflareTunnelTokenKey+" is required"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: %s is required; export it from 1Password env or your shell before running `make dev-ephemeral`",
			hostname,
			localteststack.CloudflareTunnelTokenKey,
		)
	case strings.Contains(message, localteststack.CloudflareHostnameKey+" is required"):
		return fmt.Sprintf(
			"start cloudflare tunnel: %s is required; export it from 1Password env or your shell before running `make dev-ephemeral`",
			localteststack.CloudflareHostnameKey,
		)
	case strings.Contains(message, "start cloudflared"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: cloudflared not found; install it locally, then rerun `make dev-ephemeral`",
			hostname,
		)
	case strings.Contains(message, "cloudflared exited"):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: cloudflared exited before becoming ready; check the token, hostname, and Cloudflare tunnel routing",
			hostname,
		)
	case strings.Contains(message, "invalid "+localteststack.CloudflareHostnameKey):
		return fmt.Sprintf(
			"start cloudflare tunnel for %s: invalid %s; use a bare hostname such as `mesh.example.test`",
			hostname,
			localteststack.CloudflareHostnameKey,
		)
	default:
		return fmt.Sprintf("start cloudflare tunnel for %s: %v", hostname, err)
	}
}

func startupInterrupted(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	log.Printf("startup interrupted: %v", ctx.Err())
	return true
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

func waitForLocalAgent(ctx context.Context, server *controlplane.Server, agentID string, runErrCh <-chan error) error {
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for {
		select {
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("agent %s did not connect within 15s", agentID)
			}
			return pollCtx.Err()
		case err := <-runErrCh:
			if err == nil || ctx.Err() != nil {
				return context.Canceled
			}
			return fmt.Errorf("agent %s exited before connecting: %w", agentID, err)
		default:
		}

		healthy, err := server.HasHealthyAgent(pollCtx, agentID)
		if err != nil {
			return err
		}
		if healthy {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func monitorBackgroundComponent(ctx context.Context, stop context.CancelFunc, name string, runErrCh <-chan error) {
	err := <-runErrCh
	if err == nil || ctx.Err() != nil {
		return
	}
	log.Printf("%s exited: %v", name, err)
	if stop != nil {
		stop()
	}
}
