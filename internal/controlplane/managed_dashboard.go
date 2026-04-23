package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type ManagedDashboardReconciler struct {
	cfg       config.ManagedDashboardConfig
	database  config.DatabaseConfig
	store     *Store
	authority *TLSAuthority
	ingress   *IngressSyncer
}

func NewManagedDashboardReconciler(
	cfg config.ManagedDashboardConfig,
	database config.DatabaseConfig,
	store *Store,
	authority *TLSAuthority,
	ingress *IngressSyncer,
) *ManagedDashboardReconciler {
	if !cfg.Enabled {
		return nil
	}
	return &ManagedDashboardReconciler{
		cfg:       cfg,
		database:  database,
		store:     store,
		authority: authority,
		ingress:   ingress,
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
	identity, err := r.authority.EnsureClientIdentity(serviceCallerDashboard, r.cfg.ServiceCallerID)
	if err != nil {
		return fmt.Errorf("ensure dashboard client identity: %w", err)
	}
	spec := directImageServiceSpec(r.cfg.Image, &platformv1.ServiceRuntime{
		Command:         append([]string(nil), r.cfg.Command...),
		Args:            append([]string(nil), r.cfg.Args...),
		Env:             r.dashboardEnv(identity),
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
	service, err := r.store.ensureManagedService(ctx, project.ID, r.cfg.ServiceName, spec)
	if err != nil {
		return fmt.Errorf("ensure dashboard managed service: %w", err)
	}
	if _, err := r.store.ensureManagedDomainBinding(ctx, project.ID, r.cfg.PublicDomain, service.ID, r.cfg.ContainerPort); err != nil {
		return fmt.Errorf("ensure dashboard domain binding: %w", err)
	}
	slog.Info("managed dashboard reconciled", "project_id", project.ID, "service_id", service.ID, "domain", r.cfg.PublicDomain)
	return r.ingress.Sync(ctx)
}

func (r *ManagedDashboardReconciler) dashboardEnv(identity ClientIdentityMaterial) map[string]string {
	env := make(map[string]string, len(r.cfg.Env)+10)
	for key, value := range r.cfg.Env {
		env[key] = value
	}
	env["DASHBOARD_DATABASE_URL"] = r.database.URL
	env["DASHBOARD_DATABASE_SCHEMA"] = r.cfg.DatabaseSchema
	env["DASHBOARD_SESSION_COOKIE_NAME"] = r.cfg.SessionCookieName
	env["DASHBOARD_JWT_SECRET"] = r.cfg.JWTSecret
	if _, ok := env["DASHBOARD_PUBLIC_BASE_URL"]; !ok {
		env["DASHBOARD_PUBLIC_BASE_URL"] = publicBaseURL(r.cfg.PublicDomain)
	}
	if r.cfg.GitHubInstallURL != "" {
		env["DASHBOARD_GITHUB_INSTALL_URL"] = r.cfg.GitHubInstallURL
	}
	env["DASHBOARD_INGRESS_TARGET_HOST"] = r.cfg.IngressTargetHost
	env["DASHBOARD_CONTROLPLANE_ADDRESS"] = r.cfg.ControlPlaneAddr
	env["DASHBOARD_CONTROLPLANE_SERVER_NAME"] = r.cfg.ControlPlaneSNI
	env["DASHBOARD_CONTROLPLANE_CA_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.CAPEM)
	env["DASHBOARD_CONTROLPLANE_CERT_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.CertPEM)
	env["DASHBOARD_CONTROLPLANE_KEY_PEM_B64"] = base64.StdEncoding.EncodeToString(identity.KeyPEM)
	if devUsers := formatDashboardUsers(r.cfg.DevUsers); devUsers != "" {
		env["DASHBOARD_DEV_USERS"] = devUsers
	}
	return env
}

func formatDashboardUsers(users []config.BootstrapUser) string {
	if len(users) == 0 {
		return ""
	}
	items := make([]string, 0, len(users))
	for _, user := range users {
		subject := strings.TrimSpace(user.Subject)
		email := strings.TrimSpace(user.Email)
		if subject == "" || email == "" {
			continue
		}
		items = append(items, subject+":"+email)
	}
	return strings.Join(items, ";")
}

func publicBaseURL(domain string) string {
	domain = strings.TrimSpace(domain)
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		return domain
	}
	return "https://" + domain
}
