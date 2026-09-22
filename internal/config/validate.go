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
	if len(cfg.ReplicaAddresses) > 1 && strings.TrimSpace(cfg.AdvertiseAddr) == "" {
		return errors.New("controlplane.advertiseAddr is required when multiple replica addresses are configured")
	}
	if err := validateServerTLS("controlplane.internalGrpc.tls", cfg.InternalGRPC.TLS); err != nil {
		return err
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
	if err := validateSourceArchives(cfg.SourceArchives); err != nil {
		return err
	}
	if err := validateSecretKeys(cfg.SecretKeys); err != nil {
		return err
	}
	if cfg.Deletion.GracePeriodDays <= 0 {
		return errors.New("controlplane.deletion.gracePeriodDays must be greater than 0")
	}
	if cfg.Deletion.GCIntervalSeconds <= 0 {
		return errors.New("controlplane.deletion.gcIntervalSeconds must be greater than 0")
	}
	if cfg.Ingress.PublicAddr == "" {
		return errors.New("controlplane.ingress.publicAddr is required")
	}
	if err := validateXDSListen(cfg.Ingress); err != nil {
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
		// Pull capabilities refresh on every Sync stream, and agent
		// sessions rotate at least every client-certificate lifetime, so
		// the pull TTL must exceed it or steady-state pulls fail.
		if cfg.Registry.PullCredentialTTLSeconds <= cfg.InternalGRPC.TLS.ClientCertValidityHours*3600 {
			return errors.New("controlplane.registry.pullCredentialTTLSeconds must exceed controlplane.internalGrpc.tls.clientCertValidityHours")
		}
	}
	if cfg.Builder.HeartbeatTimeoutSeconds <= 0 {
		return errors.New("controlplane.builder.heartbeatTimeoutSeconds must be greater than 0")
	}
	if cfg.Builder.MaxAttempts <= 0 {
		return errors.New("controlplane.builder.maxAttempts must be greater than 0")
	}
	if cfg.Builder.MaxConcurrentGlobal <= 0 {
		return errors.New("controlplane.builder.maxConcurrentGlobal must be greater than 0")
	}
	if cfg.Builder.MaxConcurrentPerProject <= 0 {
		return errors.New("controlplane.builder.maxConcurrentPerProject must be greater than 0")
	}
	if cfg.Builder.BuildTimeoutSeconds <= 0 {
		return errors.New("controlplane.builder.buildTimeoutSeconds must be greater than 0")
	}
	if cfg.Builder.MaxQueueAgeSeconds <= 0 {
		return errors.New("controlplane.builder.maxQueueAgeSeconds must be greater than 0")
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
	if err := validateRoutableUnicast("agent.node.advertiseAddr", cfg.Node.AdvertiseAddr); err != nil {
		return err
	}
	if cfg.Node.Resources.CPUMillis <= 0 {
		return errors.New("agent.node.resources.cpuMillis must be greater than 0")
	}
	if cfg.Node.Resources.MemoryMebibytes <= 0 {
		return errors.New("agent.node.resources.memoryMebibytes must be greater than 0")
	}
	if cfg.Node.Resources.ReservedCPUMillis < 0 {
		return errors.New("agent.node.resources.reservedCpuMillis must not be negative")
	}
	if cfg.Node.Resources.ReservedMemoryMebibytes < 0 {
		return errors.New("agent.node.resources.reservedMemoryMebibytes must not be negative")
	}
	if cfg.Node.Resources.AdvertisedCPUMillis() <= 0 {
		return errors.New("agent.node.resources.reservedCpuMillis must be less than cpuMillis")
	}
	if cfg.Node.Resources.AdvertisedMemoryMebibytes() <= 0 {
		return errors.New("agent.node.resources.reservedMemoryMebibytes must be less than memoryMebibytes")
	}
	if len(cfg.ControlPlane.Addresses) == 0 {
		return errors.New("agent.controlPlane.addresses is required")
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
	if err := validateRoutableUnicast("agent.mesh.host.ipv6", cfg.Mesh.Host.IPv6); err != nil {
		return err
	}
	hostAddr, err := netip.ParseAddr(strings.TrimSpace(cfg.Mesh.Host.IPv6))
	if err != nil {
		return fmt.Errorf("agent.mesh.host.ipv6 must be IPv6: %q", strings.TrimSpace(cfg.Mesh.Host.IPv6))
	}
	advertiseAddr, err := netip.ParseAddr(strings.TrimSpace(cfg.Node.AdvertiseAddr))
	if err != nil {
		return fmt.Errorf("agent.node.advertiseAddr must be IPv6: %q", strings.TrimSpace(cfg.Node.AdvertiseAddr))
	}
	if hostAddr.Unmap() != advertiseAddr.Unmap() {
		return errors.New("agent.mesh.host.ipv6 must match agent.node.advertiseAddr")
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
	if err := validateWireGuardEndpoint(cfg.Mesh.WireGuard.AdvertiseEndpoint); err != nil {
		return fmt.Errorf("agent.mesh.wireguard.advertiseEndpoint is invalid: %w", err)
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
		if err := validateWireGuardEndpoint(peer.Endpoint); err != nil {
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
	if cfg.BuildctlBinary == "" {
		return errors.New("builder.buildctlBinary is required")
	}
	if cfg.BuildkitAddress == "" {
		return errors.New("builder.buildkitAddress is required")
	}
	if cfg.RailpackBinary == "" {
		return errors.New("builder.railpackBinary is required")
	}
	if cfg.RailpackFrontendImage == "" {
		return errors.New("builder.railpackFrontendImage is required")
	}
	switch cfg.Executor {
	case "development", "hardened":
	case "":
		return errors.New("builder.executor is required")
	default:
		return fmt.Errorf("builder.executor must be %q or %q", "development", "hardened")
	}
	if cfg.Executor == "hardened" {
		if err := validateBuilderSandbox(cfg.Sandbox); err != nil {
			return err
		}
	}
	if cfg.Limits.TimeoutSeconds <= 0 {
		return errors.New("builder.limits.timeoutSeconds must be greater than 0")
	}
	if cfg.Limits.MemoryBytes <= 0 {
		return errors.New("builder.limits.memoryBytes must be greater than 0")
	}
	if cfg.Limits.CPUSeconds <= 0 {
		return errors.New("builder.limits.cpuSeconds must be greater than 0")
	}
	if cfg.Limits.MaxFileBytes <= 0 {
		return errors.New("builder.limits.maxFileBytes must be greater than 0")
	}
	if cfg.Limits.MaxProcesses <= 0 {
		return errors.New("builder.limits.maxProcesses must be greater than 0")
	}
	if cfg.Limits.MaxWorkspaceBytes <= 0 {
		return errors.New("builder.limits.maxWorkspaceBytes must be greater than 0")
	}
	for _, raw := range cfg.Network.DeniedCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("builder.network.deniedCidrs must be valid CIDRs: %q", raw)
		}
	}
	switch cfg.Cache.Mode {
	case "", "none", "content-addressed":
	default:
		return fmt.Errorf("builder.cache.mode must be %q or %q", "none", "content-addressed")
	}
	if cfg.Profile.IsProduction() {
		return validateProductionBuilder(cfg)
	}
	return nil
}

func validateBuilderSandbox(cfg BuilderSandboxConfig) error {
	if cfg.Backend != "containerd" {
		return fmt.Errorf("builder.sandbox.backend must be %q", "containerd")
	}
	for field, value := range map[string]string{
		"builder.sandbox.socket":          cfg.Socket,
		"builder.sandbox.namespace":       cfg.Namespace,
		"builder.sandbox.image":           cfg.Image,
		"builder.sandbox.runtime":         cfg.Runtime,
		"builder.sandbox.snapshotter":     cfg.Snapshotter,
		"builder.sandbox.cniPluginDir":    cfg.CNIPluginDir,
		"builder.sandbox.cniConfDir":      cfg.CNIConfDir,
		"builder.sandbox.cniNetwork":      cfg.CNINetwork,
		"builder.sandbox.buildkitdBinary": cfg.BuildkitdBinary,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required for the hardened executor", field)
		}
	}
	for _, nameserver := range cfg.Nameservers {
		if ip := net.ParseIP(strings.TrimSpace(nameserver)); ip == nil {
			return fmt.Errorf("builder.sandbox.nameservers must be valid IPs: %q", nameserver)
		}
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

func validateWireGuardEndpoint(raw string) error {
	raw = strings.TrimSpace(raw)
	endpoint, err := netip.ParseAddrPort(raw)
	if err != nil {
		if _, port, splitErr := net.SplitHostPort(raw); splitErr == nil && (port == "" || strings.Trim(port, "0123456789") != "") {
			return fmt.Errorf("port must be 1-65535: %q", raw)
		}
		return fmt.Errorf("host must be an IP:port endpoint: %q", raw)
	}
	if endpoint.Port() == 0 {
		return fmt.Errorf("port must be 1-65535: %q", raw)
	}
	return validateRoutableAddr("host", endpoint.Addr().Unmap())
}

func validateRoutableUnicast(field, raw string) error {
	raw = strings.TrimSpace(raw)
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return fmt.Errorf("%s must be a routable unicast address: %q", field, raw)
	}
	return validateRoutableAddr(field, addr.Unmap())
}

func validateRoutableAddr(field string, addr netip.Addr) error {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() ||
		addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return fmt.Errorf("%s must be a routable unicast address: %q", field, addr)
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

func validateXDSListen(cfg IngressConfig) error {
	if strings.TrimSpace(cfg.XDSListen) == "" {
		return errors.New("controlplane.ingress.xdsListen is required")
	}
	if _, _, err := net.SplitHostPort(cfg.XDSListen); err != nil {
		return fmt.Errorf("controlplane.ingress.xdsListen must be host:port: %w", err)
	}
	return nil
}

func validateSourceArchives(cfg SourceArchiveConfig) error {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = SourceArchiveProviderFile
	}
	if cfg.RetentionDays <= 0 {
		return errors.New("controlplane.sourceArchives.retentionDays must be greater than 0")
	}
	switch provider {
	case SourceArchiveProviderFile:
		if strings.TrimSpace(cfg.Directory) == "" {
			return errors.New("controlplane.sourceArchives.directory is required")
		}
		if strings.TrimSpace(cfg.S3.Endpoint) != "" || strings.TrimSpace(cfg.S3.Region) != "" ||
			strings.TrimSpace(cfg.S3.Bucket) != "" || strings.TrimSpace(cfg.S3.Prefix) != "" ||
			strings.TrimSpace(cfg.S3.ServerSideEncryption) != "" || strings.TrimSpace(cfg.S3.SSEKMSKeyID) != "" ||
			strings.TrimSpace(cfg.S3.CredentialsFile) != "" {
			return errors.New("controlplane.sourceArchives.s3 fields require provider s3")
		}
		return nil
	case SourceArchiveProviderS3:
		if strings.TrimSpace(cfg.Directory) != "" {
			return errors.New("controlplane.sourceArchives.directory must be empty when provider is s3")
		}
		return validateSourceArchiveS3(cfg.S3)
	default:
		return fmt.Errorf("controlplane.sourceArchives.provider must be %q or %q", SourceArchiveProviderFile, SourceArchiveProviderS3)
	}
}

func validateSourceArchiveS3(cfg SourceArchiveS3Config) error {
	if err := validateAbsoluteURL("controlplane.sourceArchives.s3.endpoint", cfg.Endpoint); err != nil {
		return err
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil {
		return fmt.Errorf("controlplane.sourceArchives.s3.endpoint must be a valid absolute URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("controlplane.sourceArchives.s3.endpoint must use http or https")
	}
	if strings.TrimSpace(cfg.Region) == "" || strings.ContainsAny(strings.TrimSpace(cfg.Region), " \t\n/") {
		return errors.New("controlplane.sourceArchives.s3.region is required")
	}
	if err := validateS3BucketName(cfg.Bucket); err != nil {
		return err
	}
	if strings.HasPrefix(strings.TrimSpace(cfg.Prefix), "/") {
		return errors.New("controlplane.sourceArchives.s3.prefix must not start with /")
	}
	switch strings.TrimSpace(cfg.ServerSideEncryption) {
	case "", "AES256", "aws:kms":
	default:
		return errors.New("controlplane.sourceArchives.s3.serverSideEncryption must be empty, AES256, or aws:kms")
	}
	if strings.TrimSpace(cfg.ServerSideEncryption) == "AES256" && strings.TrimSpace(cfg.SSEKMSKeyID) != "" {
		return errors.New("controlplane.sourceArchives.s3.kmsKeyId requires aws:kms encryption")
	}
	if cfg.RequestTimeoutSeconds < 1 || cfg.RequestTimeoutSeconds > 300 {
		return errors.New("controlplane.sourceArchives.s3.requestTimeoutSeconds must be between 1 and 300")
	}
	if cfg.MaxRetries < 1 || cfg.MaxRetries > 10 {
		return errors.New("controlplane.sourceArchives.s3.maxRetries must be between 1 and 10")
	}
	return nil
}

func validateS3BucketName(bucket string) error {
	bucket = strings.TrimSpace(bucket)
	if len(bucket) < 3 || len(bucket) > 63 {
		return errors.New("controlplane.sourceArchives.s3.bucket must be 3-63 characters")
	}
	for _, r := range bucket {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return errors.New("controlplane.sourceArchives.s3.bucket must be lowercase letters, digits, hyphens, or dots")
	}
	if strings.Contains(bucket, "..") || strings.HasPrefix(bucket, "-") || strings.HasSuffix(bucket, "-") ||
		strings.HasPrefix(bucket, ".") || strings.HasSuffix(bucket, ".") {
		return errors.New("controlplane.sourceArchives.s3.bucket is not a valid bucket name")
	}
	return nil
}

func validateSecretKeys(cfg SecretKeysConfig) error {
	if strings.TrimSpace(cfg.KeyringPath) == "" {
		return errors.New("controlplane.secretKeys.keyringPath is required")
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
