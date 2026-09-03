package firewall

import (
	"fmt"
	"net/netip"

	"ebof-wg-mesh/internal/config"
)

type configuredIdentity struct {
	prefix          netip.Prefix
	networkIdentity uint32
	hostIPv6        netip.Addr
}

func configuredIdentities(cfg config.MeshRuntimeConfig) ([]configuredIdentity, error) {
	ipv4Pool, err := netip.ParsePrefix(cfg.WorkloadIPv4PoolCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse IPv4 workload pool cidr %q: %w", cfg.WorkloadIPv4PoolCIDR, err)
	}
	if !ipv4Pool.Addr().Is4() {
		return nil, fmt.Errorf("IPv4 workload pool cidr must be IPv4: %q", cfg.WorkloadIPv4PoolCIDR)
	}
	ipv6Pool, err := netip.ParsePrefix(cfg.WorkloadPoolCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse workload pool cidr %q: %w", cfg.WorkloadPoolCIDR, err)
	}
	if !ipv6Pool.Addr().Is6() {
		return nil, fmt.Errorf("workload pool cidr must be IPv6: %q", cfg.WorkloadPoolCIDR)
	}
	ipv4Pool = ipv4Pool.Masked()
	ipv6Pool = ipv6Pool.Masked()
	// Environment network identity zero is never assigned to a tenant. This less-specific
	// LPM entry makes an unknown workload-pool destination resolve to a tenant mismatch
	// in the eBPF policy; known workload entries override it for both families.
	identities := []configuredIdentity{{prefix: ipv4Pool}, {prefix: ipv6Pool}}
	for _, seed := range cfg.Containerd.IdentitySeeds {
		workloadIPv4, err := netip.ParseAddr(seed.IPv4)
		if err != nil {
			return nil, fmt.Errorf("parse containerd identity seed IPv4 %q: %w", seed.IPv4, err)
		}
		if !workloadIPv4.Is4() {
			return nil, fmt.Errorf("containerd identity seed IPv4 must be IPv4: %q", seed.IPv4)
		}
		workloadIPv6, err := netip.ParseAddr(seed.IPv6)
		if err != nil {
			return nil, fmt.Errorf("parse containerd identity seed ip %q: %w", seed.IPv6, err)
		}
		if !workloadIPv6.Is6() {
			return nil, fmt.Errorf("containerd identity seed ip must be IPv6: %q", seed.IPv6)
		}
		hostIP, err := netip.ParseAddr(seed.HostIPv6)
		if err != nil {
			return nil, fmt.Errorf("parse containerd identity seed host ip %q: %w", seed.HostIPv6, err)
		}
		if !hostIP.Is6() {
			return nil, fmt.Errorf("containerd identity seed host ip must be IPv6: %q", seed.HostIPv6)
		}
		if seed.NetworkIdentity == 0 {
			return nil, fmt.Errorf("containerd identity seed %s has zero environment network identity", seed.IPv6)
		}
		identity := configuredIdentity{
			networkIdentity: seed.NetworkIdentity,
			hostIPv6:        hostIP,
		}
		identity.prefix = netip.PrefixFrom(workloadIPv4, 32)
		identities = append(identities, identity)
		identity.prefix = netip.PrefixFrom(workloadIPv6, 128)
		identities = append(identities, identity)
	}
	return identities, nil
}
