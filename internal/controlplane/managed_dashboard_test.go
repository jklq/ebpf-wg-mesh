package controlplane

import (
	"strings"
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestManagedDashboardDesiredEnvironmentContainsOnlySecretFileReferences(t *testing.T) {
	t.Parallel()

	reconciler := &ManagedDashboardReconciler{cfg: config.ManagedDashboardConfig{
		DatabaseSchema:    "dashboard",
		SessionCookieName: "dashboard_session",
		ControlPlaneAddr:  "controlplane:9443",
		ControlPlaneSNI:   "controlplane",
	}}
	env, err := reconciler.dashboardEnv()
	if err != nil {
		t.Fatalf("dashboardEnv: %v", err)
	}
	for _, key := range []string{
		"DASHBOARD_DATABASE_URL",
		"DASHBOARD_JWT_SECRET",
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET",
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY",
		"DASHBOARD_CONTROLPLANE_KEY_PEM_B64",
	} {
		if _, exists := env[key]; exists {
			t.Fatalf("secret %s was included in desired state", key)
		}
	}
	for _, key := range []string{
		"DASHBOARD_DATABASE_URL_FILE",
		"DASHBOARD_JWT_SECRET_FILE",
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET_FILE",
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY_FILE",
		"DASHBOARD_CONTROLPLANE_KEY_FILE",
	} {
		if !strings.HasPrefix(env[key], managedDashboardSecretsMount) {
			t.Fatalf("secret file %s is not under the managed mount: %q", key, env[key])
		}
	}
}

func TestManagedDashboardRejectsSecretsInConfiguredEnvironment(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"DASHBOARD_DATABASE_URL",
		"DASHBOARD_JWT_SECRET",
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET",
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY",
		"DASHBOARD_CONTROLPLANE_KEY_PEM_B64",
		"DASHBOARD_GITHUB_CLIENT_SECRET",
	} {
		reconciler := &ManagedDashboardReconciler{cfg: config.ManagedDashboardConfig{
			Env: map[string]string{key: "secret"},
		}}
		if _, err := reconciler.dashboardEnv(); err == nil {
			t.Fatalf("expected %s to be rejected", key)
		}
	}
}
