package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
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
	"ebof-wg-mesh/internal/testdb"
	"ebof-wg-mesh/internal/testutil"
)

type stackSummary struct {
	ControlPlaneURL   string             `json:"control_plane_url"`
	DashboardURL      string             `json:"dashboard_url"`
	DatabaseURL       string             `json:"database_url"`
	ClickHouseURL     string             `json:"clickhouse_url"`
	RegistryURL       string             `json:"registry_url"`
	ArtifactsDir      string             `json:"artifacts_dir"`
	PublicBaseURL     string             `json:"public_base_url"`
	GitHubEnabled     bool               `json:"github_enabled"`
	GitHubCallbackURL string             `json:"github_callback_url"`
	GitHubWebhookURL  string             `json:"github_webhook_url"`
	ProductE2E        *productE2ESummary `json:"product_e2e,omitempty"`
}

const consoleStartupTimeout = 2 * time.Minute

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
	// Drop stale summary so Playwright cannot race a previous run's session cookie
	// against a freshly generated JWT secret once /healthz becomes ready.
	_ = os.Remove(filepath.Join(artifactsDir, "stack.json"))

	cockroach, err := testdb.Start("")
	if err != nil {
		log.Fatalf("start cockroach testserver: %v", err)
	}
	defer cockroach.Stop()

	cockroachURL := testdb.NormalizeURL(cockroach.PGURL())
	if cockroachURL == nil {
		log.Fatal("nil cockroach pg url")
	}
	dbURL := cockroachURL.String()
	stateDir := filepath.Join(os.TempDir(), fmt.Sprintf("ebpf-wg-mesh-localtest-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Fatalf("mkdir state dir: %v", err)
	}
	defer os.RemoveAll(stateDir)
	// A killed previous run leaves fixed-name containers and published ports
	// (especially :8080). Clean them before reserving host ports or starting
	// managed containers so health waits do not hang on a wedged bind.
	if removed, err := localteststack.CleanupStaleLocalteststackContainers(ctx, localteststack.ExecDockerRunner{}); err != nil {
		log.Fatalf("cleanup stale localteststack containers: %v", err)
	} else if removed > 0 {
		log.Printf("removed %d stale localteststack container(s) from a previous run", removed)
	}
	// Hold the console listen port until vite/bun starts. pick-and-release leaves
	// a long race with Docker port publishes while cockroach/ingress/agent boot.
	consoleListener, consolePort, err := reservePort(stackCfg.ConsoleBindAddress)
	if err != nil {
		log.Fatalf("reserve console port: %v", err)
	}
	defer consoleListener.Close()
	ingressAdminPort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick ingress admin port: %v", err)
	}
	clickHousePort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick clickhouse port: %v", err)
	}
	registryPort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick registry port: %v", err)
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
	dashboardJWTSecret, err := randomSecret(32)
	if err != nil {
		log.Fatalf("generate dashboard JWT secret: %v", err)
	}
	userAssertionSecret, err := randomSecret(32)
	if err != nil {
		log.Fatalf("generate user assertion secret: %v", err)
	}
	githubTokenEncryptionKey, err := randomSecret(32)
	if err != nil {
		log.Fatalf("generate GitHub token encryption key: %v", err)
	}
	agentBootstrapToken, err := randomSecret(32)
	if err != nil {
		log.Fatalf("generate agent bootstrap token: %v", err)
	}
	bootstrapUsers := []config.BootstrapUser{{
		ID:       "dev-user",
		Email:    "dev@example.com",
		Operator: true,
	}}
	if stackCfg.OperatorGitHubLogin != "" {
		bootstrapUsers = append(bootstrapUsers, config.BootstrapUser{
			ID:       operatorGitHubUserID(stackCfg.OperatorGitHubLogin),
			Email:    stackCfg.OperatorGitHubLogin + "@users.noreply.github.com",
			Operator: true,
		})
	}

	cfg := config.ControlPlaneConfig{
		Profile: config.ProfileDevelopment,
		InternalGRPC: config.ListenerConfig{
			Listen: "127.0.0.1:0",
			TLS: config.ServerTLSConfig{
				ServerNames: []string{"controlplane", "localhost"},
				BootstrapTokens: []config.AgentBootstrapToken{{
					AgentID:                 localAgentID,
					Token:                   agentBootstrapToken,
					Name:                    "Local Teststack Agent",
					Region:                  "local",
					FailureDomain:           "localteststack",
					ReservedCPUMillis:       500,
					ReservedMemoryMebibytes: 512,
				}},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		UserAssertions: config.UserAssertionConfig{HMACSecret: userAssertionSecret},
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
			AdminURL:                ingress.AdminURL(),
			AdminListen:             ":2019",
			AllowNonLoopbackAdmin:   true,
			ListenAddrs:             []string{fmt.Sprintf(":%d", stackCfg.IngressPort)},
			DisableAutomaticHTTPS:   true,
			PublicAddr:              stackCfg.IngressHost,
			UseReportedAllocationIP: true,
			StaticRoutes: []config.StaticIngressRouteConfig{{
				Hosts:    []string{stackCfg.IngressHost},
				Upstream: fmt.Sprintf("host.docker.internal:%d", consolePort),
			}},
			ControlPlaneHTTPUpstream: "127.0.0.1:8080",
		},
		Bootstrap: config.BootstrapConfig{
			Users: bootstrapUsers,
		},
		Dashboard: config.ManagedDashboardConfig{
			ServiceCallerID: "dashboard-local",
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
		"DASHBOARD_PROFILE":                            "development",
		"DASHBOARD_DATABASE_URL":                       dbURL,
		"DASHBOARD_DATABASE_SCHEMA":                    "dashboard_local_e2e",
		"DASHBOARD_SESSION_COOKIE_NAME":                "dashboard_local_e2e_session",
		"DASHBOARD_JWT_SECRET":                         dashboardJWTSecret,
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET": userAssertionSecret,
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY":        githubTokenEncryptionKey,
		"DASHBOARD_PUBLIC_BASE_URL":                    ingressURL[:len(ingressURL)-1],
		"DASHBOARD_LOCAL_INGRESS_BASE_URL":             ingressURL[:len(ingressURL)-1],
		"DASHBOARD_INGRESS_TARGET_HOST":                stackCfg.IngressHost,
		"DASHBOARD_LOCAL_DOMAIN_SUFFIX":                stackCfg.LocalDomainSuffix,
		"DASHBOARD_OPERATOR_GITHUB_LOGIN":              stackCfg.OperatorGitHubLogin,
	}
	// Public tunnel mode must not expose open dev logins on the internet-facing
	// hostname. Product e2e authenticates via a per-run session cookie written
	// only to the local stack artifact (signed with this run's JWT secret).
	productE2EEnabled := os.Getenv("LOCALTESTSTACK_PRODUCT_E2E") == "1"
	if !stackCfg.EnablePublicTunnel {
		consoleEnv["DASHBOARD_DEV_USERS"] = "dev-user:dev@example.com"
	} else {
		consoleEnv["DASHBOARD_DEV_USERS"] = ""
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
	missingGitHubKeys := localteststack.MissingGitHubKeys(overlayEnv)
	publicTunnelEnabled := cloudflareTunnelRequested(
		stackCfg.EnablePublicTunnel,
		len(missingGitHubKeys) == 0,
		cloudflareTunnelToken,
		cloudflareHostname,
	)
	if publicTunnelEnabled {
		if len(missingGitHubKeys) > 0 {
			log.Fatalf("public tunnel requires non-dev GitHub authentication; missing keys: %s", strings.Join(missingGitHubKeys, ", "))
		}
		if net.ParseIP(stackCfg.ConsoleBindAddress).IsLoopback() {
			log.Fatalf("public tunnel requires an explicit non-loopback LOCALTESTSTACK_CONSOLE_BIND_ADDRESS")
		}
		consoleEnv["DASHBOARD_DEV_USERS"] = ""
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
	// Optional separate platform domain suffix keeps generated service hosts on
	// free Universal SSL (e.g. *.relay5.com) while the dashboard/public base
	// stays on a non-apex tunnel hostname (e.g. mesh.relay5.com).
	if stackCfg.PlatformDomainSuffix != "" {
		cfg.Ingress.PublicAddr = stackCfg.PlatformDomainSuffix
		log.Printf("platform domain suffix: %s", stackCfg.PlatformDomainSuffix)
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
	registryHost := fmt.Sprintf("localhost:%d", registryPort)
	cfg.Registry = config.RegistryConfig{
		Host:            registryHost,
		NamespacePrefix: "mesh",
		// Registry clients run inside Docker Desktop/BuildKit and reach this
		// host process through Docker's host gateway.
		AuthListen:           "0.0.0.0:0",
		TokenIssuer:          "ebpf-wg-mesh-local",
		TokenService:         registryHost,
		CredentialTTLSeconds: 300,
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		log.Fatalf("finalize controlplane config: %v", err)
	}
	log.Print(config.ControlPlaneStartupContract(cfg).String())

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
	_, registryAuthPort, err := net.SplitHostPort(server.RegistryAuthAddr())
	if err != nil {
		log.Fatalf("resolve registry auth port: %v", err)
	}
	registryAuthUpstreamPort, err := strconv.Atoi(registryAuthPort)
	if err != nil {
		log.Fatalf("parse registry auth port: %v", err)
	}
	// Docker Desktop BuildKit can push to localhost:<published-port> (same as the
	// registry) but often cannot dial host-gateway IPs for host listeners
	// (i/o timeout) and cannot resolve host.docker.internal. Publish a tiny
	// localhost proxy container that forwards to the control-plane token service.
	registryAuthProxyPort, err := pickLoopbackPort()
	if err != nil {
		log.Fatalf("pick registry auth proxy port: %v", err)
	}
	registryAuthProxy, err := localteststack.StartManagedRegistryAuthProxy(ctx, localteststack.LocalRegistryAuthProxyConfig{
		ContainerName: "localteststack-registry-auth-proxy",
		UpstreamHost:  "host.docker.internal",
		UpstreamPort:  registryAuthUpstreamPort,
		HostPort:      registryAuthProxyPort,
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		log.Fatalf("start registry auth proxy: %v", err)
	}
	defer func() {
		if err := registryAuthProxy.Close(); err != nil {
			log.Printf("stop registry auth proxy: %v", err)
		}
	}()
	tokenRealm := registryAuthProxy.TokenRealmBaseURL() + controlplane.RegistryTokenPath
	log.Printf("registry token realm: %s (proxy -> host.docker.internal:%d)", tokenRealm, registryAuthUpstreamPort)
	registry, err := localteststack.StartManagedRegistry(ctx, localteststack.LocalRegistryConfig{
		StateDir:       filepath.Join(stateDir, "local-registry"),
		ContainerName:  "localteststack-registry",
		HostPort:       registryPort,
		TokenRealm:     tokenRealm,
		TokenService:   cfg.Registry.TokenService,
		TokenIssuer:    cfg.Registry.TokenIssuer,
		RootCertBundle: server.RegistryAuthCertificatePath(),
	}, localteststack.ExecDockerRunner{})
	if err != nil {
		log.Fatalf("start managed local registry: %v", err)
	}
	defer func() {
		if err := registry.Close(); err != nil {
			log.Printf("stop managed local registry: %v", err)
		}
	}()
	log.Printf("local registry ready: %s", registry.Host())

	identity, err := server.EnsureDashboardClientIdentity("dashboard-local")
	if err != nil {
		log.Fatalf("mint dashboard client identity: %v", err)
	}

	localAgent, localAgentErrCh, err := startLocalAgent(ctx, stackCfg, stateDir, controlPlaneURL, identity.CAPEM, agentBootstrapToken)
	if err != nil {
		log.Fatalf("start local agent: %v", err)
	}
	defer localAgent.Close()
	if err := waitForLocalAgent(ctx, server, localAgentID, localAgentErrCh); err != nil {
		log.Fatalf("wait for local agent: %v", err)
	}
	log.Printf("local agent ready: %s", localAgentID)
	go monitorBackgroundComponent(ctx, stop, "local agent", localAgentErrCh)

	var productFixture *productE2ESummary
	if productE2EEnabled {
		if strings.TrimSpace(overlay.PublicBaseURL) == "" {
			log.Fatalf("local product e2e requires the public Cloudflare tunnel; set LOCALTESTSTACK_ENABLE_PUBLIC_TUNNEL=1 with CLOUDFLARE_TUNNEL_TOKEN and CLOUDFLARE_HOSTNAME")
		}
		log.Printf("running local product draft/release/domain fixture via public tunnel %s", overlay.PublicBaseURL)
		result, err := runProductE2EScenario(
			ctx,
			controlPlaneURL,
			identity,
			userAssertionSecret,
			stackCfg.IngressPort,
			overlay.PublicBaseURL,
			consoleEnv["DASHBOARD_SESSION_COOKIE_NAME"],
			dashboardJWTSecret,
		)
		if err != nil {
			log.Fatalf("local product e2e: %v", err)
		}
		productFixture = &result
		log.Printf("local product fixture ready: %s", result.RouteURL)
		// Seed before the console becomes healthy so Playwright cannot start
		// against /healthz with a missing dashboard user or stale stack.json.
		if err := seedProductE2EDashboardUser(
			ctx,
			dbURL,
			consoleEnv["DASHBOARD_DATABASE_SCHEMA"],
			productE2EUserID,
			productE2EUserEmail,
		); err != nil {
			log.Fatalf("seed product e2e dashboard user: %v", err)
		}
		log.Printf("product e2e dashboard user seeded: %s", productE2EUserID)
	}

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

	// Publish stack.json before /healthz is up so Playwright always reads this run.
	summary := stackSummary{
		ControlPlaneURL:   controlPlaneURL,
		DashboardURL:      ingressURL,
		DatabaseURL:       dbURL,
		ClickHouseURL:     clickHouseURL,
		RegistryURL:       "http://" + registry.Host(),
		ArtifactsDir:      artifactsDir,
		PublicBaseURL:     firstNonEmpty(overlay.PublicBaseURL, ingressURL[:len(ingressURL)-1]),
		GitHubEnabled:     overlay.GitHubEnabled,
		GitHubCallbackURL: overlay.GitHubCallbackURL,
		GitHubWebhookURL:  overlay.GitHubWebhookURL,
		ProductE2E:        productFixture,
	}
	if err := writeSummary(filepath.Join(artifactsDir, "stack.json"), summary); err != nil {
		log.Fatalf("write stack summary: %v", err)
	}

	consoleProc, err := startConsole(ctx, consoleDir, consoleEnv, consolePort, stackCfg.ConsoleBindAddress, consoleListener)
	if err != nil {
		log.Fatalf("start console: %v", err)
	}
	// startConsole closes the reserved listener before binding the real server.
	defer stopConsoleProcess(consoleProc)
	if err := waitForIngressDashboard(ctx, ingressURL+"healthz"); err != nil {
		log.Fatalf("wait for ingress dashboard health: %v", err)
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
