package localteststack

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"

	onepassword "github.com/1password/onepassword-sdk-go"
)

func TestSelectAuthPrefersServiceAccountToken(t *testing.T) {
	t.Parallel()

	auth := SelectAuth(EnvironmentLoaderConfig{
		ServiceAccountToken: "token-value",
		Account:             "desktop-account",
	})
	if auth.Method != AuthMethodServiceAccount {
		t.Fatalf("expected service account auth, got %s", auth.Method)
	}
	if auth.ServiceAccountToken != "token-value" {
		t.Fatalf("unexpected token %q", auth.ServiceAccountToken)
	}
	if auth.Account != "" {
		t.Fatalf("expected desktop account to be ignored, got %q", auth.Account)
	}
}

func TestVariablesToMapRejectsDuplicateNormalizedKeys(t *testing.T) {
	t.Parallel()

	_, err := variablesToMap([]onepassword.EnvironmentVariable{
		{Name: "CONTROLPLANE_GITHUB_APP_ID", Value: "1"},
		{Name: " CONTROLPLANE_GITHUB_APP_ID ", Value: "2"},
	})
	if err == nil {
		t.Fatal("expected duplicate key error")
	}
	if got := err.Error(); got != `duplicate key "CONTROLPLANE_GITHUB_APP_ID"` {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestApplyEnvironmentOverlayWithoutOnePasswordConfigLeavesDefaults(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, nil, "")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if result.GitHubEnabled {
		t.Fatal("expected GitHub to remain disabled")
	}
	if result.PublicBaseURL != "" {
		t.Fatalf("expected no public base URL, got %q", result.PublicBaseURL)
	}
	if cfg.Ingress.PublicAddr != "platform.localtest.me" {
		t.Fatalf("unexpected ingress public addr %q", cfg.Ingress.PublicAddr)
	}
	if dashboardEnv[DashboardPublicBaseURLKey] != "http://platform.localtest.me:8080" {
		t.Fatalf("unexpected dashboard public base URL %q", dashboardEnv[DashboardPublicBaseURLKey])
	}
	if dashboardEnv[DashboardIngressTargetHostKey] != "platform.localtest.me" {
		t.Fatalf("unexpected dashboard ingress host %q", dashboardEnv[DashboardIngressTargetHostKey])
	}
}

func TestApplyEnvironmentOverlayPrefersLocalteststackPublicURL(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{
		ControlPlaneGitHubAppIDKey:            "123",
		ControlPlaneGitHubWebhookSecretKey:    "webhook-secret",
		ControlPlaneGitHubPrivateKeyPEMKey:    sampleGitHubPrivateKeyPEMBase64(),
		ControlPlaneDashboardGitHubInstallKey: "https://github.com/apps/demo/installations/new",
		ControlPlaneRegistryHostKey:           "ghcr.io",
		ControlPlaneRegistryUsernameKey:       "registry-user",
		ControlPlaneRegistryPasswordKey:       "registry-password",
		DashboardGitHubAppIDKey:               "123",
		DashboardGitHubClientIDKey:            "client-id",
		DashboardGitHubClientSecretKey:        "client-secret",
	}, "https://mesh.example.test")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if result.PublicBaseURL != "https://mesh.example.test" {
		t.Fatalf("unexpected public base URL %q", result.PublicBaseURL)
	}
	if !result.GitHubEnabled || !cfg.GitHub.Enabled {
		t.Fatal("expected GitHub to be enabled")
	}
	if cfg.GitHub.PrivateKeyPEM != sampleGitHubPrivateKeyPEM() {
		t.Fatalf("unexpected private key pem %q", cfg.GitHub.PrivateKeyPEM)
	}
	if cfg.Ingress.PublicAddr != "mesh.example.test" {
		t.Fatalf("unexpected ingress public addr %q", cfg.Ingress.PublicAddr)
	}
	if dashboardEnv[DashboardPublicBaseURLKey] != "https://mesh.example.test" {
		t.Fatalf("unexpected dashboard public base URL %q", dashboardEnv[DashboardPublicBaseURLKey])
	}
	if dashboardEnv[DashboardIngressTargetHostKey] != "platform.localtest.me" {
		t.Fatalf("unexpected dashboard ingress host %q", dashboardEnv[DashboardIngressTargetHostKey])
	}
	if result.GitHubCallbackURL != "https://mesh.example.test/auth/callback" {
		t.Fatalf("unexpected callback URL %q", result.GitHubCallbackURL)
	}
	if result.GitHubWebhookURL != "https://mesh.example.test/webhooks/github" {
		t.Fatalf("unexpected webhook URL %q", result.GitHubWebhookURL)
	}
}

func TestApplyEnvironmentOverlayAppliesExternalPublicURLWithoutGitHub(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, nil, "https://mesh.example.test")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if result.GitHubEnabled {
		t.Fatal("expected GitHub to remain disabled")
	}
	if result.PublicBaseURL != "https://mesh.example.test" {
		t.Fatalf("unexpected public base URL %q", result.PublicBaseURL)
	}
	if cfg.Ingress.PublicAddr != "mesh.example.test" {
		t.Fatalf("unexpected ingress public addr %q", cfg.Ingress.PublicAddr)
	}
	if dashboardEnv[DashboardPublicBaseURLKey] != "https://mesh.example.test" {
		t.Fatalf("unexpected dashboard public base URL %q", dashboardEnv[DashboardPublicBaseURLKey])
	}
}

func TestApplyEnvironmentOverlayEnablesGitHubWithoutInstallURL(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{
		ControlPlaneGitHubAppIDKey:         "123",
		ControlPlaneGitHubWebhookSecretKey: "webhook-secret",
		ControlPlaneGitHubPrivateKeyPEMKey: sampleGitHubPrivateKeyPEMBase64(),
		ControlPlaneRegistryHostKey:        "ghcr.io",
		ControlPlaneRegistryUsernameKey:    "registry-user",
		ControlPlaneRegistryPasswordKey:    "registry-password",
		DashboardGitHubAppIDKey:            "123",
		DashboardGitHubClientIDKey:         "client-id",
		DashboardGitHubClientSecretKey:     "client-secret",
	}, "https://mesh.example.test")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if !result.GitHubEnabled {
		t.Fatal("expected GitHub to be enabled without an install URL")
	}
	if _, ok := dashboardEnv[DashboardGitHubInstallURLKey]; ok {
		t.Fatal("expected install URL to remain unset when not provided")
	}
}

func TestApplyEnvironmentOverlayReportsCloudflareRuntimeRequirementWhenGitHubSecretsAreComplete(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{
		ControlPlaneGitHubAppIDKey:            "123",
		ControlPlaneGitHubWebhookSecretKey:    "webhook-secret",
		ControlPlaneGitHubPrivateKeyPEMKey:    sampleGitHubPrivateKeyPEMBase64(),
		ControlPlaneDashboardGitHubInstallKey: "https://github.com/apps/demo/installations/new",
		ControlPlaneRegistryHostKey:           "ghcr.io",
		ControlPlaneRegistryUsernameKey:       "registry-user",
		ControlPlaneRegistryPasswordKey:       "registry-password",
		DashboardGitHubAppIDKey:               "123",
		DashboardGitHubClientIDKey:            "client-id",
		DashboardGitHubClientSecretKey:        "client-secret",
	}, "")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if result.GitHubEnabled {
		t.Fatal("expected GitHub to remain disabled without a public URL source")
	}
	if len(result.MissingRuntimeKeys) != 2 || result.MissingRuntimeKeys[0] != CloudflareTunnelTokenKey || result.MissingRuntimeKeys[1] != CloudflareHostnameKey {
		t.Fatalf("unexpected runtime keys %+v", result.MissingRuntimeKeys)
	}
	if cfg.GitHub.Enabled {
		t.Fatal("expected control plane GitHub config to remain disabled")
	}
}

func TestStartCloudflareTunnelRequiresToken(t *testing.T) {
	t.Parallel()

	_, err := StartCloudflareTunnel(context.Background(), "", "hostname.example.test")
	if err == nil {
		t.Fatal("expected cloudflare tunnel token error")
	}
	if got := err.Error(); got != "CLOUDFLARE_TUNNEL_TOKEN is required" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestStartCloudflareTunnelRequiresHostname(t *testing.T) {
	t.Parallel()

	_, err := StartCloudflareTunnel(context.Background(), "token", "")
	if err == nil {
		t.Fatal("expected cloudflare hostname error")
	}
	if got := err.Error(); got != "CLOUDFLARE_HOSTNAME is required" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestStartCloudflareTunnelReturnsHostnameWhenProcessStarts(t *testing.T) {
	binDir := t.TempDir()
	cloudflaredPath := filepath.Join(binDir, "cloudflared")
	if err := os.WriteFile(cloudflaredPath, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatalf("write fake cloudflared: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	result, err := StartCloudflareTunnel(ctx, "token", "mesh.dev.example.test")
	if err != nil {
		t.Fatalf("StartCloudflareTunnel: %v", err)
	}
	defer result.Close()

	if result.BaseURL != "https://mesh.dev.example.test" {
		t.Fatalf("unexpected base URL %q", result.BaseURL)
	}
	if result.Host != "mesh.dev.example.test" {
		t.Fatalf("unexpected host %q", result.Host)
	}
}

func TestStartCloudflareTunnelSurfacesMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := StartCloudflareTunnel(context.Background(), "token", "mesh.dev.example.test")
	if err == nil {
		t.Fatal("expected missing binary error")
	}
	if got := err.Error(); got != `start cloudflared: exec: "cloudflared": executable file not found in $PATH` {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestWaitForCommandStartupSucceedsWhileProcessIsStillRunning(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = stopCommand(cmd)
	}()

	if err := waitForCommandStartup(ctx, cmd, 100*time.Millisecond); err != nil {
		t.Fatalf("waitForCommandStartup: %v", err)
	}
}

func TestWaitForCommandStartupReturnsEarlyExit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 17")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := waitForCommandStartup(ctx, cmd, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected startup failure")
	}
	if got := err.Error(); got != "cloudflared exited: exit status 17" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestApplyEnvironmentOverlayRejectsInvalidPublicURL(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	_, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{}, "http://mesh.example.test")
	if err == nil {
		t.Fatal("expected invalid public URL error")
	}
	if got := err.Error(); got != "invalid public base url \"http://mesh.example.test\": must use https" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestApplyEnvironmentOverlayKeepsGitHubDisabledForPartialContract(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{
		ControlPlaneGitHubAppIDKey:            "123",
		ControlPlaneRegistryHostKey:           "ghcr.io",
		DashboardGitHubAppIDKey:               "123",
		DashboardGitHubClientIDKey:            "client-id",
		DashboardGitHubClientSecretKey:        "client-secret",
		ControlPlaneGitHubWebhookPathKey:      "/custom/webhook",
		ControlPlaneGitHubWebhookSecretKey:    "webhook-secret",
		ControlPlaneGitHubPrivateKeyPEMKey:    sampleGitHubPrivateKeyPEMBase64(),
		ControlPlaneRegistryUsernameKey:       "registry-user",
		ControlPlaneDashboardGitHubInstallKey: "https://github.com/apps/demo/installations/new",
	}, "https://mesh.example.test")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if result.GitHubEnabled {
		t.Fatal("expected GitHub to remain disabled")
	}
	if cfg.GitHub.Enabled {
		t.Fatal("expected control plane GitHub config to remain disabled")
	}
	if len(result.MissingGitHubKeys) == 0 {
		t.Fatal("expected missing GitHub keys to be reported")
	}
	if result.GitHubWebhookURL != "https://mesh.example.test/custom/webhook" {
		t.Fatalf("unexpected webhook URL %q", result.GitHubWebhookURL)
	}
	if _, ok := dashboardEnv[DashboardGitHubClientIDKey]; ok {
		t.Fatal("expected dashboard GitHub env to remain unset for partial contract")
	}
}

func TestApplyEnvironmentOverlayEnablesGitHubForCompleteContract(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	dashboardEnv := baseDashboardEnv()

	result, err := ApplyEnvironmentOverlay(&cfg, dashboardEnv, map[string]string{
		ControlPlaneGitHubAppIDKey:             "123",
		ControlPlaneGitHubWebhookSecretKey:     "webhook-secret",
		ControlPlaneGitHubPrivateKeyPEMKey:     sampleGitHubPrivateKeyPEMBase64(),
		ControlPlaneRegistryHostKey:            "ghcr.io",
		ControlPlaneRegistryNamespacePrefixKey: "mesh-dev",
		ControlPlaneRegistryUsernameKey:        "registry-user",
		ControlPlaneRegistryPasswordKey:        "registry-password",
		ControlPlaneGitHubAPIBaseURLKey:        "https://api.github.example.test",
		ControlPlaneGitHubWebBaseURLKey:        "https://github.example.test",
		ControlPlaneGitHubWebhookPathKey:       "/hooks/github",
		DashboardGitHubAppIDKey:                "123",
		DashboardGitHubClientIDKey:             "client-id",
		DashboardGitHubClientSecretKey:         "client-secret",
		DashboardGitHubAuthBaseURLKey:          "https://github.example.test",
		DashboardGitHubAPIBaseURLKey:           "https://api.github.example.test",
	}, "https://mesh.dev.example.test")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if !result.GitHubEnabled {
		t.Fatal("expected GitHub to be enabled")
	}
	if cfg.Ingress.PublicAddr != "mesh.dev.example.test" {
		t.Fatalf("unexpected ingress public addr %q", cfg.Ingress.PublicAddr)
	}
	if !cfg.GitHub.Enabled || cfg.GitHub.AppID != 123 {
		t.Fatalf("unexpected GitHub config %+v", cfg.GitHub)
	}
	if cfg.GitHub.WebhookPath != "/hooks/github" {
		t.Fatalf("unexpected webhook path %q", cfg.GitHub.WebhookPath)
	}
	if cfg.Registry.Host != "ghcr.io" || cfg.Registry.Username != "registry-user" || cfg.Registry.Password != "registry-password" {
		t.Fatalf("unexpected registry config %+v", cfg.Registry)
	}
	if cfg.Registry.NamespacePrefix != "mesh-dev" {
		t.Fatalf("unexpected registry namespace prefix %q", cfg.Registry.NamespacePrefix)
	}
	if dashboardEnv[DashboardGitHubClientIDKey] != "client-id" {
		t.Fatalf("unexpected dashboard client id %q", dashboardEnv[DashboardGitHubClientIDKey])
	}
	if result.GitHubCallbackURL != "https://mesh.dev.example.test/auth/callback" {
		t.Fatalf("unexpected callback URL %q", result.GitHubCallbackURL)
	}
	if result.GitHubWebhookURL != "https://mesh.dev.example.test/hooks/github" {
		t.Fatalf("unexpected webhook URL %q", result.GitHubWebhookURL)
	}
}

func sampleGitHubPrivateKeyPEM() string {
	return "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"
}

func sampleGitHubPrivateKeyPEMBase64() string {
	return base64.StdEncoding.EncodeToString([]byte(sampleGitHubPrivateKeyPEM()))
}

func baseConfig() config.ControlPlaneConfig {
	return config.ControlPlaneConfig{
		Ingress: config.IngressConfig{
			PublicAddr: "platform.localtest.me",
		},
	}
}

func baseDashboardEnv() map[string]string {
	return map[string]string{
		DashboardPublicBaseURLKey:     "http://platform.localtest.me:8080",
		DashboardIngressTargetHostKey: "platform.localtest.me",
	}
}
