//go:build integration

package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/localteststack"
)

func TestOnePasswordEnvironmentProvidesGitHubDevstackContract(t *testing.T) {
	if _, err := localteststack.LoadDotEnvFile(filepath.Join("..", "..", ".env")); err != nil {
		t.Fatalf("load .env: %v", err)
	}

	vars := localteststack.OverlayEnvFromLookup(os.Getenv)
	loader := localteststack.NewEnvironmentLoader(localteststack.EnvironmentLoaderConfigFromLookup(os.Getenv))
	if len(localteststack.MissingGitHubKeys(vars)) > 0 {
		if !loader.Enabled() {
			t.Skip("GitHub devstack environment is not configured in .env, process environment, or 1Password")
		}
		if !loader.Ready() {
			t.Skipf(
				"%s is set but neither %s nor %s is configured",
				localteststack.OPEnvironmentIDKey,
				localteststack.OPServiceAccountTokenKey,
				localteststack.OPAccountKey,
			)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		remoteVars, err := loader.Load(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		vars = localteststack.MergeOverlayEnv(remoteVars, vars)
	}

	if missing := localteststack.MissingGitHubKeys(vars); len(missing) > 0 {
		t.Fatalf("missing required GitHub devstack keys: %v", missing)
	}

	tunnelToken := vars[localteststack.CloudflareTunnelTokenKey]
	hostname := vars[localteststack.CloudflareHostnameKey]
	if tunnelToken == "" || hostname == "" {
		t.Fatalf(
			"missing required localteststack tunnel runtime keys: %s=%t %s=%t",
			localteststack.CloudflareTunnelTokenKey,
			tunnelToken != "",
			localteststack.CloudflareHostnameKey,
			hostname != "",
		)
	}
	publicBaseURL := "https://" + hostname

	cfg := config.ControlPlaneConfig{
		Ingress: config.IngressConfig{
			PublicAddr: "platform.localtest.me",
		},
	}
	dashboardEnv := map[string]string{
		localteststack.DashboardPublicBaseURLKey:     "http://platform.localtest.me:8080",
		localteststack.DashboardIngressTargetHostKey: "platform.localtest.me",
	}
	result, err := localteststack.ApplyEnvironmentOverlay(&cfg, dashboardEnv, vars, publicBaseURL)
	if err != nil {
		t.Fatalf("ApplyEnvironmentOverlay: %v", err)
	}
	if !result.GitHubEnabled {
		t.Fatal("expected GitHub devstack contract to enable GitHub")
	}
	if result.GitHubCallbackURL == "" || result.GitHubWebhookURL == "" {
		t.Fatalf("expected derived callback and webhook URLs, got %+v", result)
	}
}
