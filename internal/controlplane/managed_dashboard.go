package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

const managedDashboardSecretsMount = "/run/secrets/dashboard"

type ManagedDashboardReconciler struct {
	cfg      config.ManagedDashboardConfig
	store    *Store
	ingress  *IngressSyncer
	notifier *Notifier
}

func NewManagedDashboardReconciler(
	cfg config.ManagedDashboardConfig,
	store *Store,
	ingress *IngressSyncer,
	notifier *Notifier,
) *ManagedDashboardReconciler {
	if !cfg.Enabled {
		return nil
	}
	return &ManagedDashboardReconciler{
		cfg:      cfg,
		store:    store,
		ingress:  ingress,
		notifier: notifier,
	}
}

func (r *ManagedDashboardReconciler) Reconcile(ctx context.Context) error {
	if r == nil {
		return nil
	}
	project, err := r.store.ensureManagedProject(ctx, r.cfg.ProjectName, r.cfg.ProjectSystemKey)
	if err != nil {
		return fmt.Errorf("ensure dashboard managed project: %w", err)
	}
	env, err := r.dashboardEnv()
	if err != nil {
		return err
	}
	spec := directImageServiceSpec(r.cfg.Image, &platformv1.ServiceRuntime{
		Command:         append([]string(nil), r.cfg.Command...),
		Args:            append([]string(nil), r.cfg.Args...),
		Env:             env,
		CpuMillis:       r.cfg.CPUMillis,
		MemoryMebibytes: r.cfg.MemoryMebibytes,
		Ports:           runtimePortsFromInts([]int32{r.cfg.ContainerPort}),
	})
	if r.cfg.HealthPath != "" {
		spec.Runtime.HealthCheck = &platformv1.HealthCheck{
			Type:           platformv1.HealthCheck_TYPE_HTTP,
			Path:           r.cfg.HealthPath,
			TimeoutSeconds: 2,
		}
	}
	service, affectedAgentIDs, err := r.store.ensureManagedService(ctx, project.ID, r.cfg.ServiceName, spec, r.cfg.TrustedAgentID)
	if err != nil {
		return fmt.Errorf("ensure dashboard managed service: %w", err)
	}
	for _, agentID := range affectedAgentIDs {
		if r.notifier != nil {
			r.notifier.Notify(agentID)
		}
	}
	if _, err := r.store.ensureManagedDomainBinding(ctx, project.ID, r.cfg.PublicDomain, service.ID, r.cfg.ContainerPort); err != nil {
		return fmt.Errorf("ensure dashboard domain binding: %w", err)
	}
	slog.Info("managed dashboard reconciled", "project_id", project.ID, "service_id", service.ID, "domain", r.cfg.PublicDomain)
	return r.ingress.Sync(ctx)
}

func (r *ManagedDashboardReconciler) dashboardEnv() (map[string]string, error) {
	env := make(map[string]string, len(r.cfg.Env)+12)
	for key, value := range r.cfg.Env {
		if managedDashboardSecretEnvKey(key) {
			return nil, fmt.Errorf("managed dashboard secret %s must be injected out-of-band", key)
		}
		env[key] = value
	}
	env["DASHBOARD_DATABASE_SCHEMA"] = r.cfg.DatabaseSchema
	env["DASHBOARD_SESSION_COOKIE_NAME"] = r.cfg.SessionCookieName
	env["DASHBOARD_DATABASE_URL_FILE"] = managedDashboardSecretsMount + "/database-url"
	env["DASHBOARD_JWT_SECRET_FILE"] = managedDashboardSecretsMount + "/jwt-secret"
	env["DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET_FILE"] = managedDashboardSecretsMount + "/user-assertion-secret"
	env["DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY_FILE"] = managedDashboardSecretsMount + "/github-token-encryption-key"
	env["DASHBOARD_CONTROLPLANE_CA_FILE"] = managedDashboardSecretsMount + "/controlplane-ca.pem"
	env["DASHBOARD_CONTROLPLANE_CERT_FILE"] = managedDashboardSecretsMount + "/controlplane-cert.pem"
	env["DASHBOARD_CONTROLPLANE_KEY_FILE"] = managedDashboardSecretsMount + "/controlplane-key.pem"
	if env["DASHBOARD_GITHUB_CLIENT_ID"] != "" {
		env["DASHBOARD_GITHUB_CLIENT_SECRET_FILE"] = managedDashboardSecretsMount + "/github-client-secret"
	}
	env["PLATFORM_MANAGED_SECRET_SET"] = "dashboard"
	if _, ok := env["DASHBOARD_PUBLIC_BASE_URL"]; !ok {
		env["DASHBOARD_PUBLIC_BASE_URL"] = publicBaseURL(r.cfg.PublicDomain)
	}
	if r.cfg.GitHubInstallURL != "" {
		env["DASHBOARD_GITHUB_INSTALL_URL"] = r.cfg.GitHubInstallURL
	}
	env["DASHBOARD_INGRESS_TARGET_HOST"] = r.cfg.IngressTargetHost
	env["DASHBOARD_CONTROLPLANE_ADDRESS"] = r.cfg.ControlPlaneAddr
	env["DASHBOARD_CONTROLPLANE_SERVER_NAME"] = r.cfg.ControlPlaneSNI
	return env, nil
}

func managedDashboardSecretEnvKey(key string) bool {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "DASHBOARD_DATABASE_URL",
		"DASHBOARD_JWT_SECRET",
		"DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET",
		"DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY",
		"DASHBOARD_CONTROLPLANE_KEY_PEM_B64",
		"DASHBOARD_GITHUB_CLIENT_SECRET":
		return true
	default:
		return false
	}
}

func publicBaseURL(domain string) string {
	domain = strings.TrimSpace(domain)
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return domain
	}
	return "https://" + domain
}
