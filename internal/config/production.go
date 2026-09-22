package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func validateProductionControlPlane(cfg ControlPlaneConfig) error {
	if err := validateProductionTLSIdentity("controlplane.internalGrpc.tls.serverNames", cfg.InternalGRPC.TLS.ServerNames); err != nil {
		return err
	}
	if err := validateProductionDurableTarget("controlplane.database.url", cfg.Database.URL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Logs.ClickHouse.URL) != "" {
		if err := validateProductionDurableTarget("controlplane.logs.clickhouse.url", cfg.Logs.ClickHouse.URL); err != nil {
			return err
		}
	}
	if err := validateProductionSourceArchives(cfg.SourceArchives); err != nil {
		return err
	}
	if err := validateProductionDurablePath("controlplane.secretKeys.keyringPath", cfg.SecretKeys.KeyringPath); err != nil {
		return err
	}
	if err := validateProductionPublicHost("controlplane.ingress.publicAddr", cfg.Ingress.PublicAddr); err != nil {
		return err
	}
	// TLS termination arrives with the domain/certificate lifecycle (2.8),
	// which pushes materials over SDS. Until then Envoy serves plaintext.
	if err := validateProductionXDSListen(cfg.Ingress); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Health.Listen) == "" {
		return errors.New("controlplane.health.listen is required in production")
	}
	if len(cfg.Dashboard.DevUsers) > 0 {
		return errors.New("controlplane.dashboard.devUsers are not allowed in production")
	}
	if err := validateProductionDashboard(cfg.Dashboard); err != nil {
		return err
	}
	if cfg.Registry.Host != "" {
		if err := validateProductionPublicHost("controlplane.registry.host", hostnameFromDialTarget(cfg.Registry.Host)); err != nil {
			return err
		}
	}
	return nil
}

func validateProductionAgent(cfg AgentConfig) error {
	if err := validateProductionTLSName("agent.controlPlane.tls.serverName", cfg.ControlPlane.TLS.ServerName); err != nil {
		return err
	}
	for _, address := range cfg.ControlPlane.Addresses {
		if err := validateProductionDurableTarget("agent.controlPlane.addresses", address); err != nil {
			return err
		}
	}
	if cfg.Runtime.DisableCgroups {
		return errors.New("agent.runtime.disableCgroups is not allowed in production")
	}
	if strings.TrimSpace(cfg.Health.Listen) == "" {
		return errors.New("agent.health.listen is required in production")
	}
	return nil
}

func validateProductionBuilder(cfg BuilderConfig) error {
	if cfg.Executor == "development" {
		return errors.New("builder.executor must not be development in production: the development executor does not isolate untrusted code")
	}
	if err := validateProductionTLSName("builder.controlPlane.tls.serverName", cfg.ControlPlane.TLS.ServerName); err != nil {
		return err
	}
	if err := validateProductionDurableTarget("builder.controlPlane.address", cfg.ControlPlane.Address); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Health.Listen) == "" {
		return errors.New("builder.health.listen is required in production")
	}
	return nil
}

func validateProductionDashboard(cfg ManagedDashboardConfig) error {
	if !cfg.Enabled {
		if insecureDashboardCookieEnv(cfg.Env) {
			return errors.New("controlplane.dashboard insecure cookies are not allowed in production")
		}
		return nil
	}
	if err := validateProductionPublicHost("controlplane.dashboard.publicDomain", hostnameFromDialTarget(cfg.PublicDomain)); err != nil {
		return err
	}
	if err := validateProductionTLSName("controlplane.dashboard.controlPlaneSni", cfg.ControlPlaneSNI); err != nil {
		return err
	}
	if publicBase := strings.TrimSpace(cfg.Env["DASHBOARD_PUBLIC_BASE_URL"]); publicBase != "" {
		if err := validateProductionHTTPSURL("controlplane.dashboard.env.DASHBOARD_PUBLIC_BASE_URL", publicBase); err != nil {
			return err
		}
	}
	if insecureDashboardCookieEnv(cfg.Env) {
		return errors.New("controlplane.dashboard insecure cookies are not allowed in production")
	}
	return nil
}

func insecureDashboardCookieEnv(env map[string]string) bool {
	if env == nil {
		return false
	}
	if strings.TrimSpace(env["DASHBOARD_LOCAL_DOMAIN_SUFFIX"]) != "" {
		return true
	}
	publicBase := strings.TrimSpace(env["DASHBOARD_PUBLIC_BASE_URL"])
	return publicBase != "" && !strings.HasPrefix(strings.ToLower(publicBase), "https://")
}

func validateProductionXDSListen(cfg IngressConfig) error {
	host, _, err := net.SplitHostPort(cfg.XDSListen)
	if err != nil {
		return fmt.Errorf("controlplane.ingress.xdsListen must be host:port: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("controlplane.ingress.xdsListen must bind an explicit host in production")
	}
	return nil
}

func validateProductionTLSIdentity(field string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("%s is required in production", field)
	}
	for _, name := range names {
		if err := validateProductionTLSName(field, name); err != nil {
			return err
		}
	}
	return nil
}

func validateProductionTLSName(field, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%s is required in production", field)
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("%s must not use a wildcard identity in production", field)
	}
	return nil
}

func validateProductionDurableTarget(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s is required in production", field)
	}
	return validateProductionDurableHost(field, hostnameFromDialTarget(raw))
}

func validateProductionDurableHost(field, host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("%s is required in production", field)
	}
	if isLoopbackHost(host) {
		return fmt.Errorf("%s must not use a loopback host as a durable service in production", field)
	}
	return nil
}

func validateProductionPublicHost(field, host string) error {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, ".")
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return fmt.Errorf("%s is required in production", field)
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("%s must not use a wildcard identity in production", field)
	}
	if isLoopbackHost(host) {
		return fmt.Errorf("%s must not use a loopback host in production", field)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("%s must be a complete public DNS name in production", field)
	}
	if strings.HasSuffix(strings.ToLower(host), ".local") {
		return fmt.Errorf("%s must be a complete public DNS name in production", field)
	}
	return nil
}

func validateProductionHTTPSURL(field, raw string) error {
	if err := validateAbsoluteHTTPSURL(field, raw); err != nil {
		return err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return validateProductionPublicHost(field, parsed.Hostname())
}

func validateProductionSourceArchives(cfg SourceArchiveConfig) error {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = SourceArchiveProviderFile
	}
	if provider != SourceArchiveProviderS3 {
		return errors.New("controlplane.sourceArchives.provider must be s3 in production; the filesystem provider is for development only")
	}
	if strings.TrimSpace(cfg.S3.Endpoint) == "" || strings.TrimSpace(cfg.S3.Region) == "" || strings.TrimSpace(cfg.S3.Bucket) == "" {
		return errors.New("controlplane.sourceArchives.s3 endpoint, region, and bucket are required in production")
	}
	if err := validateProductionDurableTarget("controlplane.sourceArchives.s3.endpoint", cfg.S3.Endpoint); err != nil {
		return err
	}
	return nil
}

func validateProductionDurablePath(field, raw string) error {
	path := filepath.Clean(strings.TrimSpace(raw))
	if path == "" || path == "." {
		return fmt.Errorf("%s is required in production", field)
	}
	if !filepath.IsAbs(path) || isEphemeralPath(path) {
		return fmt.Errorf("%s must not use ephemeral storage in production", field)
	}
	return nil
}

func isEphemeralPath(path string) bool {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return true
	}
	tmp := filepath.Clean(os.TempDir())
	if path == tmp || strings.HasPrefix(path, tmp+string(os.PathSeparator)) {
		return true
	}
	return path == "/tmp" || strings.HasPrefix(path, "/tmp"+string(os.PathSeparator))
}

func hostnameFromDialTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err == nil {
			if parsed.Hostname() != "" {
				return parsed.Hostname()
			}
			if parsed.Host != "" {
				return parsed.Host
			}
		}
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host
	}
	return raw
}

func dependencyClassForURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return DependencyUnset
	}
	if isLoopbackHost(hostnameFromDialTarget(raw)) {
		return DependencyLoopback
	}
	return DependencyDurable
}

func dependencyClassForSourceArchives(cfg SourceArchiveConfig) string {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = SourceArchiveProviderFile
	}
	if provider == SourceArchiveProviderS3 {
		return dependencyClassForURL(strings.TrimSpace(cfg.S3.Endpoint))
	}
	return dependencyClassForPath(cfg.Directory)
}

func dependencyClassForPath(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return DependencyUnset
	}
	if isEphemeralPath(raw) {
		return DependencyEphemeral
	}
	return DependencyDurable
}

func dependencyClassForSecretKeys(cfg SecretKeysConfig) string {
	return dependencyClassForPath(cfg.KeyringPath)
}

func dependencyClassForAdmin(cfg IngressConfig) string {
	host, _, err := net.SplitHostPort(cfg.XDSListen)
	if err != nil {
		return DependencyUnset
	}
	if isLoopbackHost(host) {
		return DependencyLoopback
	}
	return DependencyRemote
}
