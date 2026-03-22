package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

func validateControlPlane(cfg ControlPlaneConfig) error {
	if cfg.InternalGRPC.Listen == "" {
		return errors.New("controlplane.internalGrpc.listen is required")
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
	if cfg.StateDir == "" {
		return errors.New("controlplane.stateDir is required")
	}
	if cfg.Ingress.PublicAddr == "" {
		return errors.New("controlplane.ingress.publicAddr is required")
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
		if cfg.Dashboard.PublicDomain == "" {
			return errors.New("controlplane.dashboard.publicDomain is required when dashboard is enabled")
		}
		if cfg.Dashboard.ControlPlaneAddr == "" {
			return errors.New("controlplane.dashboard.controlPlaneAddr is required when dashboard is enabled")
		}
		if strings.TrimSpace(cfg.Dashboard.JWTSecret) == "" {
			return errors.New("controlplane.dashboard.jwtSecret is required when dashboard is enabled")
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
		if err := validateAbsoluteURL("controlplane.github.webBaseUrl", cfg.GitHub.WebBaseURL); err != nil {
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
		if cfg.Registry.Username == "" {
			return errors.New("controlplane.registry.username is required when GitHub is enabled")
		}
		if cfg.Registry.Password == "" {
			return errors.New("controlplane.registry.password is required when GitHub is enabled")
		}
	}
	if cfg.Builder.HeartbeatTimeoutSeconds <= 0 {
		return errors.New("controlplane.builder.heartbeatTimeoutSeconds must be greater than 0")
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
	if cfg.Mesh.Host.IPv6 == "" {
		return errors.New("agent.mesh.host.ipv6 is required")
	}
	ip := net.ParseIP(cfg.Mesh.Host.IPv6)
	if !isIPv6(ip) {
		return fmt.Errorf("agent.mesh.host.ipv6 must be IPv6: %q", cfg.Mesh.Host.IPv6)
	}
	for _, seed := range cfg.Containerd.IdentitySeeds {
		if ip := net.ParseIP(seed.IPv6); !isIPv6(ip) {
			return fmt.Errorf("agent.containerd.identitySeeds ipv6 must be IPv6: %q", seed.IPv6)
		}
		if ip := net.ParseIP(seed.HostIPv6); !isIPv6(ip) {
			return fmt.Errorf("agent.containerd.identitySeeds hostIpv6 must be IPv6: %q", seed.HostIPv6)
		}
	}
	for _, assignment := range cfg.Containerd.StaticAssignments {
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
			if ip, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("agent.mesh.wireguard peer %q invalid allowed IP %q: %w", peer.Name, cidr, err)
			} else if !isIPv6(ip) {
				return fmt.Errorf("agent.mesh.wireguard peer %q allowedIPs must be IPv6 CIDRs: %q", peer.Name, cidr)
			}
		}
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
	if cfg.ServerCertValidityHours <= 0 {
		return fmt.Errorf("%s.serverCertValidityHours must be greater than 0", prefix)
	}
	if cfg.ClientCertValidityHours <= 0 {
		return fmt.Errorf("%s.clientCertValidityHours must be greater than 0", prefix)
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
