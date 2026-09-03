package firewall

import (
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestConfiguredIdentitiesSeedsDenyPrefixAndKnownWorkloads(t *testing.T) {
	t.Parallel()

	identities, err := configuredIdentities(config.MeshRuntimeConfig{
		WorkloadIPv4PoolCIDR: "10.200.0.0/16",
		WorkloadPoolCIDR:     "fd00:200::/48",
		Containerd: config.ContainerdConfig{IdentitySeeds: []config.IdentitySeed{{
			IPv4:            "10.200.1.10",
			IPv6:            "fd00:200:1::10",
			HostIPv6:        "2001:db8::2",
			NetworkIdentity: 9,
		}}},
	})
	if err != nil {
		t.Fatalf("configuredIdentities: %v", err)
	}
	if len(identities) != 4 {
		t.Fatalf("expected deny prefixes plus IPv4 and IPv6 workload identities, got %d", len(identities))
	}
	if got := identities[0]; got.prefix.String() != "10.200.0.0/16" || got.networkIdentity != 0 || got.hostIPv6.IsValid() {
		t.Fatalf("unexpected deny-by-default IPv4 workload prefix: %+v", got)
	}
	if got := identities[1]; got.prefix.String() != "fd00:200::/48" || got.networkIdentity != 0 || got.hostIPv6.IsValid() {
		t.Fatalf("unexpected deny-by-default IPv6 workload prefix: %+v", got)
	}
	if got := identities[2]; got.prefix.String() != "10.200.1.10/32" || got.networkIdentity != 9 || got.hostIPv6.String() != "2001:db8::2" {
		t.Fatalf("unexpected known IPv4 workload identity: %+v", got)
	}
	if got := identities[3]; got.prefix.String() != "fd00:200:1::10/128" || got.networkIdentity != 9 || got.hostIPv6.String() != "2001:db8::2" {
		t.Fatalf("unexpected known IPv6 workload identity: %+v", got)
	}
}
