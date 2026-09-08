package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
)

func validateControlPlane(cfg ControlPlaneConfig) error {
	if cfg.InternalGRPC.Listen == "" {
		return errors.New("controlplane.internalGrpc.listen is required")
	}
	if err := validateServerTLS("controlplane.internalGrpc.tls", cfg.InternalGRPC.TLS); err != nil {
		return err
	}
	if len(cfg.UserAssertions.HMACSecret) < 32 {
		return errors.New("controlplane.userAssertions.hmacSecret must be at least 32 bytes")
	}
	if cfg.Database.URL == "" {
		return errors.New("controlplane.database.url is required")
	}
	if cfg.Database.MaxOpenConns < 0 {
		return errors.New("controlplane.database.maxOpenConns must be non-negative")
	}
	if cfg.Database.MaxIdleConns < 0 {
		return errors.New("controlplane.database.maxIdleConns must be non-negative")
	}
	if cfg.Logs.RetentionDays <= 0 {
		return errors.New("controlplane.logs.retentionDays must be greater than 0")
	}
	if cfg.Logs.ClickHouse.URL != "" {
		if err := validateAbsoluteURL("controlplane.logs.clickhouse.url", cfg.Logs.ClickHouse.URL); err != nil {
			return err
		}
		if cfg.Logs.ClickHouse.MaxOpenConns < 0 {
			return errors.New("controlplane.logs.clickhouse.maxOpenConns must be non-negative")
		}
		if cfg.Logs.ClickHouse.MaxIdleConns < 0 {
			return errors.New("controlplane.logs.clickhouse.maxIdleConns must be non-negative")
		}
	}
	if cfg.StateDir == "" {
		return errors.New("controlplane.stateDir is required")
	}
	if strings.TrimSpace(cfg.SourceArchives.Directory) == "" {
		return errors.New("controlplane.sourceArchives.directory is required")
	}
	if cfg.SourceArchives.RetentionDays <= 0 {
		return errors.New("controlplane.sourceArchives.retentionDays must be greater than 0")
	}
	if cfg.Ingress.PublicAddr == "" {
		return errors.New("controlplane.ingress.publicAddr is required")
	}
	if err := validateCaddyAdmin(cfg.Ingress); err != nil {
		return err
	}
	for _, addr := range cfg.Ingress.ListenAddrs {
		if strings.TrimSpace(addr) == "" {
			return errors.New("controlplane.ingress.listenAddrs must not contain blanks")
		}
	}
	for _, route := range cfg.Ingress.StaticRoutes {
		if strings.TrimSpace(route.Upstream) == "" {
			return errors.New("controlplane.ingress.staticRoutes upstream is required")
		}
		if len(route.Hosts) == 0 {
			return errors.New("controlplane.ingress.staticRoutes hosts are required")
		}
	}
	if cfg.Dashboard.Enabled {
		if cfg.Dashboard.Image == "" {
			return errors.New("controlplane.dashboard.image is required when dashboard is enabled")
		}
		if cfg.Dashboard.ProjectSystemKey == "" {
			return errors.New("controlplane.dashboard.projectSystemKey is required when dashboard is enabled")
		}
		if cfg.Dashboard.ServiceName == "" {
			return errors.New("controlplane.dashboard.serviceName is required when dashboard is enabled")
		}
		if strings.TrimSpace(cfg.Dashboard.ServiceCallerID) == "" {
			return errors.New("controlplane.dashboard.serviceCallerId is required when dashboard is enabled")
		}
		if strings.TrimSpace(cfg.Dashboard.TrustedAgentID) == "" {
			return errors.New("controlplane.dashboard.trustedAgentId is required when dashboard is enabled")
		}
		if cfg.Dashboard.PublicDomain == "" {
			return errors.New("controlplane.dashboard.publicDomain is required when dashboard is enabled")
		}
		if cfg.Dashboard.ControlPlaneAddr == "" {
			return errors.New("controlplane.dashboard.controlPlaneAddr is required when dashboard is enabled")
		}
		if len(cfg.Dashboard.DevUsers) != 0 {
			return errors.New("controlplane.dashboard.devUsers are not allowed when dashboard is enabled")
		}
	}
	if cfg.GitHub.Enabled {
		if cfg.GitHub.AppID <= 0 {
			return errors.New("controlplane.github.appId is required when GitHub is enabled")
		}
		if strings.TrimSpace(cfg.GitHub.WebhookSecret) == "" {
			return errors.New("controlplane.github.webhookSecret is required when GitHub is enabled")
		}
		if strings.TrimSpace(cfg.GitHub.PrivateKeyPEM) == "" {
			return errors.New("controlplane.github.privateKeyPem is required when GitHub is enabled")
		}
		if err := validateAbsoluteURL("controlplane.github.apiBaseUrl", cfg.GitHub.APIBaseURL); err != nil {
			return err
		}
		if !strings.HasPrefix(cfg.GitHub.WebhookPath, "/") {
			return errors.New("controlplane.github.webhookPath must start with /")
		}
		if strings.Contains(cfg.GitHub.WebhookPath, " ") {
			return errors.New("controlplane.github.webhookPath must not contain spaces")
		}
		if cfg.Registry.Host == "" {
			return errors.New("controlplane.registry.host is required when GitHub is enabled")
		}
	}
	if cfg.Registry.Host != "" {
		if strings.ContainsAny(cfg.Registry.Host, "/ ") {
			return errors.New("controlplane.registry.host must be a host[:port] without a path")
		}
		if _, err := net.ResolveTCPAddr("tcp", cfg.Registry.AuthListen); err != nil {
			return fmt.Errorf("controlplane.registry.authListen: %w", err)
		}
		if strings.TrimSpace(cfg.Registry.TokenIssuer) == "" {
			return errors.New("controlplane.registry.tokenIssuer is required")
		}
		if strings.TrimSpace(cfg.Registry.TokenService) == "" {
			return errors.New("controlplane.registry.tokenService is required")
		}
		if cfg.Registry.CredentialTTLSeconds < 60 || cfg.Registry.CredentialTTLSeconds > 900 {
			return errors.New("controlplane.registry.credentialTTLSeconds must be between 60 and 900")
		}
	}
	if cfg.Builder.HeartbeatTimeoutSeconds <= 0 {
		return errors.New("controlplane.builder.heartbeatTimeoutSeconds must be greater than 0")
	}
	if cfg.Failover.ReconcileIntervalSeconds <= 0 {
		return errors.New("controlplane.failover.reconcileIntervalSeconds must be greater than 0")
	}
	if cfg.Failover.UnhealthyThresholdSeconds <= 0 {
		return errors.New("controlplane.failover.unhealthyThresholdSeconds must be greater than 0")
	}
	if cfg.Mesh.InterfaceName == "" {
		return errors.New("controlplane.mesh.interfaceName is required")
	}
	if cfg.Mesh.ListenPort <= 0 || cfg.Mesh.ListenPort > 65535 {
		return errors.New("controlplane.mesh.listenPort must be 1-65535")
	}
	if err := validateCIDR("controlplane.mesh.networkCidr", cfg.Mesh.NetworkCIDR, 64); err != nil {
		return err
	}
	if err := validateCIDR("controlplane.mesh.workloadPoolCidr", cfg.Mesh.WorkloadPoolCIDR, 48); err != nil {
		return err
	}
	if err := validateIPv4Pool("controlplane.mesh.workloadIpv4PoolCidr", cfg.Mesh.WorkloadIPv4PoolCIDR, cfg.Mesh.WorkloadIPv4NodePrefixBits); err != nil {
		return err
	}
	if cfg.Profile.IsProduction() {
		return validateProductionControlPlane(cfg)
	}
	return nil
}

func validateAgent(cfg AgentConfig) error {
	if cfg.Node.ID == "" {
		return errors.New("agent.node.id is required")
	}
	if cfg.Node.Name == "" {
		return errors.New("agent.node.name is required")
	}
	if cfg.Node.AdvertiseAddr == "" {
		return errors.New("agent.node.advertiseAddr is required")
	}
	if ip := net.ParseIP(cfg.Node.AdvertiseAddr); ip == nil || !isIPv6(ip) {
		return fmt.Errorf("agent.node.advertiseAddr must be IPv6: %q", cfg.Node.AdvertiseAddr)
	}
	if cfg.Node.Resources.CPUMillis <= 0 {
		return errors.New("agent.node.resources.cpuMillis must be greater than 0")
	}
	if cfg.Node.Resources.MemoryMebibytes <= 0 {
		return errors.New("agent.node.resources.memoryMebibytes must be greater than 0")
	}
	if cfg.ControlPlane.Address == "" {
		return errors.New("agent.controlPlane.address is required")
	}
	if err := validateClientTLS("agent.controlPlane.tls", cfg.ControlPlane.TLS); err != nil {
		return err
	}
	if cfg.Runtime.ManagedDashboardSecretsDir != "" && !filepath.IsAbs(cfg.Runtime.ManagedDashboardSecretsDir) {
		return errors.New("agent.runtime.managedDashboardSecretsDir must be absolute")
	}
	if cfg.Mesh.Host.IPv6 == "" {
		return errors.New("agent.mesh.host.ipv6 is required")
	}
	ip := net.ParseIP(cfg.Mesh.Host.IPv6)
	if !isIPv6(ip) {
		return fmt.Errorf("agent.mesh.host.ipv6 must be IPv6: %q", cfg.Mesh.Host.IPv6)
	}
	for _, seed := range cfg.Containerd.IdentitySeeds {
		if ip := net.ParseIP(seed.IPv4); ip == nil || ip.To4() == nil {
			return fmt.Errorf("agent.containerd.identitySeeds ipv4 must be IPv4: %q", seed.IPv4)
		}
		if ip := net.ParseIP(seed.IPv6); !isIPv6(ip) {
			return fmt.Errorf("agent.containerd.identitySeeds ipv6 must be IPv6: %q", seed.IPv6)
		}
		if ip := net.ParseIP(seed.HostIPv6); !isIPv6(ip) {
			return fmt.Errorf("agent.containerd.identitySeeds hostIpv6 must be IPv6: %q", seed.HostIPv6)
		}
	}
	for _, assignment := range cfg.Containerd.StaticAssignments {
		if ip := net.ParseIP(assignment.IPv4); ip == nil || ip.To4() == nil {
			return fmt.Errorf("agent.containerd.staticAssignments ipv4 must be IPv4: %q", assignment.IPv4)
		}
		if ip := net.ParseIP(assignment.IPv6); !isIPv6(ip) {
			return fmt.Errorf("agent.containerd.staticAssignments ipv6 must be IPv6: %q", assignment.IPv6)
		}
	}
	if cfg.Mesh.WireGuard.ListenPort <= 0 || cfg.Mesh.WireGuard.ListenPort > 65535 {
		return errors.New("agent.mesh.wireguard.listenPort must be 1-65535")
	}
	for _, cidr := range cfg.Mesh.WireGuard.Addresses {
		if ip, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid agent.mesh.wireguard address %q: %w", cidr, err)
		} else if !isIPv6(ip) {
			return fmt.Errorf("agent.mesh.wireguard.addresses must be IPv6 CIDRs: %q", cidr)
		}
	}
	for _, peer := range cfg.Mesh.WireGuard.Peers {
		if strings.TrimSpace(peer.PublicKey) == "" {
			return fmt.Errorf("agent.mesh.wireguard peer %q missing publicKey", peer.Name)
		}
		if strings.TrimSpace(peer.Endpoint) == "" {
			return fmt.Errorf("agent.mesh.wireguard peer %q missing endpoint", peer.Name)
		}
		if err := validateIPv6Endpoint(peer.Endpoint); err != nil {
			return fmt.Errorf("agent.mesh.wireguard peer %q invalid endpoint: %w", peer.Name, err)
		}
		for _, cidr := range peer.AllowedIPs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("agent.mesh.wireguard peer %q invalid allowed IP %q: %w", peer.Name, cidr, err)
			}
		}
	}
	if cfg.Profile.IsProduction() {
		return validateProductionAgent(cfg)
	}
	return nil
}

func validateBuilder(cfg BuilderConfig) error {
	if cfg.ID == "" {
		return errors.New("builder.id is required")
	}
	if cfg.Name == "" {
		return errors.New("builder.name is required")
	}
	if cfg.ControlPlane.Address == "" {
		return errors.New("builder.controlPlane.address is required")
	}
	if cfg.ControlPlane.TLS.CAFile == "" {
		return errors.New("builder.controlPlane.tls.caFile is required")
	}
	if cfg.ControlPlane.TLS.CertFile == "" {
		return errors.New("builder.controlPlane.tls.certFile is required")
	}
	if cfg.ControlPlane.TLS.KeyFile == "" {
		return errors.New("builder.controlPlane.tls.keyFile is required")
	}
	if cfg.ControlPlane.TLS.ServerName == "" {
		return errors.New("builder.controlPlane.tls.serverName is required")
	}
	if cfg.WorkDir == "" {
		return errors.New("builder.workDir is required")
	}
	if cfg.PollIntervalSeconds <= 0 {
		return errors.New("builder.pollIntervalSeconds must be greater than 0")
	}
	if cfg.HeartbeatIntervalSeconds <= 0 {
		return errors.New("builder.heartbeatIntervalSeconds must be greater than 0")
	}
	if cfg.GitBinary == "" {
		return errors.New("builder.gitBinary is required")
	}
	if cfg.BuildctlBinary == "" {
		return errors.New("builder.buildctlBinary is required")
	}
	if cfg.BuildkitAddress == "" {
		return errors.New("builder.buildkitAddress is required")
	}
	if cfg.Profile.IsProduction() {
		return validateProductionBuilder(cfg)
	}
	return nil
}

func validateCIDR(field string, raw string, prefixBits int) error {
	ip, network, err := net.ParseCIDR(raw)
	if err != nil || network == nil {
		return fmt.Errorf("%s must be a valid IPv6 CIDR: %q", field, raw)
	}
	if !isIPv6(ip) {
		return fmt.Errorf("%s must be IPv6: %q", field, raw)
	}
	ones, bits := network.Mask.Size()
	if bits != 128 || ones != prefixBits {
		return fmt.Errorf("%s must be an IPv6 /%d CIDR: %q", field, prefixBits, raw)
	}
	return nil
}

func validateIPv4Pool(field, raw string, childPrefixBits int) error {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("%s must be a valid IPv4 CIDR: %q", field, raw)
	}
	if prefix != prefix.Masked() {
		return fmt.Errorf("%s must be a canonical IPv4 CIDR: %q", field, raw)
	}
	if childPrefixBits <= prefix.Bits() || childPrefixBits > 30 {
		return fmt.Errorf("controlplane.mesh.workloadIpv4NodePrefixBits must be greater than /%d and no larger than /30", prefix.Bits())
	}
	if childPrefixBits-prefix.Bits() > 24 {
		return fmt.Errorf("%s has too many per-node prefixes", field)
	}
	return nil
}

func isIPv6(ip net.IP) bool {
	return ip != nil && ip.To4() == nil && ip.To16() != nil
}

func validateIPv6Endpoint(raw string) error {
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !addr.Is6() {
		return fmt.Errorf("host must be IPv6: %q", host)
	}
	return nil
}

func validateServerTLS(prefix string, cfg ServerTLSConfig) error {
	if len(cfg.ServerNames) == 0 {
		return fmt.Errorf("%s.serverNames must include at least one name", prefix)
	}
	if len(cfg.BootstrapTokens) == 0 {
		return fmt.Errorf("%s.bootstrapTokens must include at least one token", prefix)
	}
	agents := make(map[string]struct{}, len(cfg.BootstrapTokens))
	tokens := make(map[string]struct{}, len(cfg.BootstrapTokens))
	for _, bootstrap := range cfg.BootstrapTokens {
		agentID := strings.TrimSpace(bootstrap.AgentID)
		token := strings.TrimSpace(bootstrap.Token)
		if agentID == "" || token == "" {
			return fmt.Errorf("%s.bootstrapTokens entries require agentID and token", prefix)
		}
		if _, exists := agents[agentID]; exists {
			return fmt.Errorf("%s.bootstrapTokens contains duplicate agentID %q", prefix, agentID)
		}
		if _, exists := tokens[token]; exists {
			return fmt.Errorf("%s.bootstrapTokens contains a token assigned more than once", prefix)
		}
		if bootstrap.ReservedCPUMillis < 0 || bootstrap.ReservedMemoryMebibytes < 0 {
			return fmt.Errorf("%s.bootstrapTokens reservations must not be negative", prefix)
		}
		agents[agentID] = struct{}{}
		tokens[token] = struct{}{}
	}
	if cfg.ServerCertValidityHours <= 0 {
		return fmt.Errorf("%s.serverCertValidityHours must be greater than 0", prefix)
	}
	if cfg.ClientCertValidityHours <= 0 {
		return fmt.Errorf("%s.clientCertValidityHours must be greater than 0", prefix)
	}
	if strings.TrimSpace(cfg.RevokedClientCertSerialsFile) == "" {
		return fmt.Errorf("%s.revokedClientCertSerialsFile is required", prefix)
	}
	return nil
}

func validateClientTLS(prefix string, cfg ClientTLSConfig) error {
	if cfg.CAFile == "" {
		return fmt.Errorf("%s.caFile is required", prefix)
	}
	if cfg.ServerName == "" {
		return fmt.Errorf("%s.serverName is required", prefix)
	}
	if cfg.BootstrapToken == "" {
		return fmt.Errorf("%s.bootstrapToken is required", prefix)
	}
	if cfg.RenewBeforeMinutes <= 0 {
		return fmt.Errorf("%s.renewBeforeMinutes must be greater than 0", prefix)
	}
	return nil
}

func validateAbsoluteURL(field string, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be a valid absolute URL: %w", field, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be a valid absolute URL: %q", field, raw)
	}
	return nil
}

func validateAbsoluteHTTPSURL(field string, raw string) error {
	if err := validateAbsoluteURL(field, raw); err != nil {
		return err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("%s must use https", field)
	}
	return nil
}

func validateCaddyAdmin(cfg IngressConfig) error {
	if err := validateAbsoluteURL("controlplane.ingress.adminUrl", cfg.AdminURL); err != nil {
		return err
	}
	adminURL, err := url.Parse(cfg.AdminURL)
	if err != nil {
		return err
	}
	if adminURL.Scheme != "http" && adminURL.Scheme != "https" {
		return errors.New("controlplane.ingress.adminUrl must use http or https")
	}
	if cfg.AllowNonLoopbackAdmin {
		return nil
	}
	if !isLoopbackHost(adminURL.Hostname()) {
		return errors.New("controlplane.ingress.adminUrl must target loopback unless allowNonLoopbackAdmin is enabled")
	}
	host, _, err := net.SplitHostPort(cfg.AdminListen)
	if err != nil {
		return fmt.Errorf("controlplane.ingress.adminListen must be host:port: %w", err)
	}
	if !isLoopbackHost(host) {
		return errors.New("controlplane.ingress.adminListen must bind loopback unless allowNonLoopbackAdmin is enabled")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
