package localteststack

import (
	"context"
	"encoding/base64"
	"net/url"
	"testing"

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

func TestApplyEnvironmentOverlayReportsNgrokRuntimeRequirementWhenGitHubSecretsAreComplete(t *testing.T) {
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
	if len(result.MissingRuntimeKeys) != 2 || result.MissingRuntimeKeys[0] != NgrokAuthtokenKey || result.MissingRuntimeKeys[1] != NgrokDomainKey {
		t.Fatalf("unexpected runtime keys %+v", result.MissingRuntimeKeys)
	}
	if cfg.GitHub.Enabled {
		t.Fatal("expected control plane GitHub config to remain disabled")
	}
}

func TestResolvePublicURLRequiresNgrokDomain(t *testing.T) {
	t.Parallel()

	_, err := ResolvePublicURLForUpstream(context.Background(), "token", "", "http://platform.localtest.me:8080", nil)
	if err == nil {
		t.Fatal("expected ngrok domain error")
	}
	if got := err.Error(); got != "NGROK_DOMAIN is required" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestResolvePublicURLRequiresNgrokAuthtoken(t *testing.T) {
	t.Parallel()

	_, err := ResolvePublicURLForUpstream(context.Background(), "", "custom.example.test", "http://platform.localtest.me:8080", nil)
	if err == nil {
		t.Fatal("expected ngrok authtoken error")
	}
	if got := err.Error(); got != "NGROK_AUTHTOKEN is required" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestResolvePublicURLStartsNgrokWhenTokenPresent(t *testing.T) {
	t.Parallel()

	starter := &fakePublicTunnelStarter{
		tunnel: fakePublicTunnel{url: mustParseURL("https://example.ngrok.app")},
	}
	result, err := ResolvePublicURLForUpstream(context.Background(), "token", "custom.example.test", "http://platform.localtest.me:8080", starter)
	if err != nil {
		t.Fatalf("ResolvePublicURL: %v", err)
	}
	if result.BaseURL != "https://example.ngrok.app" {
		t.Fatalf("unexpected base URL %q", result.BaseURL)
	}
	if starter.calls != 1 {
		t.Fatalf("expected ngrok starter to be called once, got %d calls", starter.calls)
	}
	if starter.upstream != "http://platform.localtest.me:8080" {
		t.Fatalf("unexpected upstream %q", starter.upstream)
	}
	if starter.domain != "custom.example.test" {
		t.Fatalf("unexpected domain %q", starter.domain)
	}
	if result.Host != "example.ngrok.app" {
		t.Fatalf("unexpected host %q", result.Host)
	}
}

func TestResolvePublicURLForUpstreamUsesProvidedIngressURL(t *testing.T) {
	t.Parallel()

	starter := &fakePublicTunnelStarter{
		tunnel: fakePublicTunnel{url: mustParseURL("https://example.ngrok.app")},
	}
	result, err := ResolvePublicURLForUpstream(context.Background(), "token", "custom.example.test", "http://platform.localtest.me:41234", starter)
	if err != nil {
		t.Fatalf("ResolvePublicURLForUpstream: %v", err)
	}
	if result.BaseURL != "https://example.ngrok.app" {
		t.Fatalf("unexpected base URL %q", result.BaseURL)
	}
	if starter.upstream != "http://platform.localtest.me:41234" {
		t.Fatalf("unexpected upstream %q", starter.upstream)
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
	}, "https://mesh.ngrok.app")
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if !result.GitHubEnabled {
		t.Fatal("expected GitHub to be enabled")
	}
	if cfg.Ingress.PublicAddr != "mesh.ngrok.app" {
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
	if result.GitHubCallbackURL != "https://mesh.ngrok.app/auth/callback" {
		t.Fatalf("unexpected callback URL %q", result.GitHubCallbackURL)
	}
	if result.GitHubWebhookURL != "https://mesh.ngrok.app/hooks/github" {
		t.Fatalf("unexpected webhook URL %q", result.GitHubWebhookURL)
	}
}

type fakePublicTunnelStarter struct {
	calls    int
	upstream string
	domain   string
	tunnel   fakePublicTunnel
}

func (s *fakePublicTunnelStarter) Start(_ context.Context, upstream string, domain string) (PublicTunnel, error) {
	s.calls++
	s.upstream = upstream
	s.domain = domain
	return s.tunnel, nil
}

type fakePublicTunnel struct {
	url *url.URL
}

func (t fakePublicTunnel) URL() *url.URL { return t.url }

func (t fakePublicTunnel) Close() error { return nil }

func mustParseURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
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
