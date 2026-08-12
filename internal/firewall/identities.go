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
	workloadPool, err := netip.ParsePrefix(cfg.WorkloadPoolCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse workload pool cidr %q: %w", cfg.WorkloadPoolCIDR, err)
	}
	if !workloadPool.Addr().Is6() {
		return nil, fmt.Errorf("workload pool cidr must be IPv6: %q", cfg.WorkloadPoolCIDR)
	}
	workloadPool = workloadPool.Masked()
	// Environment network identity zero is never assigned to a tenant. This less-specific
	// LPM entry makes an unknown workload-pool destination resolve to a tenant
	// mismatch in the eBPF policy; known /128 workload entries override it.
	identities := []configuredIdentity{{prefix: workloadPool}}
	for _, seed := range cfg.Containerd.IdentitySeeds {
		workloadIP, err := netip.ParseAddr(seed.IPv6)
		if err != nil {
			return nil, fmt.Errorf("parse containerd identity seed ip %q: %w", seed.IPv6, err)
		}
		if !workloadIP.Is6() {
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
		identities = append(identities, configuredIdentity{
			prefix:          netip.PrefixFrom(workloadIP, 128),
			networkIdentity: seed.NetworkIdentity,
			hostIPv6:        hostIP,
		})
	}
	return identities, nil
}
