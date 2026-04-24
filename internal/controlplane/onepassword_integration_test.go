//go:build integration

package controlplane

import (
	"context"
	"os"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/localteststack"
)

func TestOnePasswordEnvironmentProvidesGitHubDevstackContract(t *testing.T) {
	t.Parallel()

	loader := localteststack.NewEnvironmentLoader(localteststack.EnvironmentLoaderConfigFromLookup(os.Getenv))
	if !loader.Enabled() {
		t.Skipf("%s is not configured", localteststack.OPEnvironmentIDKey)
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
	defer cancel()

	vars, err := loader.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
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
