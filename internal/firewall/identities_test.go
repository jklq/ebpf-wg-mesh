package firewall

import (
	"testing"

	"ebof-wg-mesh/internal/config"
)

func TestConfiguredIdentitiesSeedsDenyPrefixAndKnownWorkloads(t *testing.T) {
	t.Parallel()

	identities, err := configuredIdentities(config.MeshRuntimeConfig{
		WorkloadPoolCIDR: "fd00:200::/48",
		Containerd: config.ContainerdConfig{IdentitySeeds: []config.IdentitySeed{{
			IPv6:            "fd00:200:1::10",
			HostIPv6:        "2001:db8::2",
			NetworkIdentity: 9,
		}}},
	})
	if err != nil {
		t.Fatalf("configuredIdentities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("expected deny prefix plus workload identity, got %d", len(identities))
	}
	if got := identities[0]; got.prefix.String() != "fd00:200::/48" || got.networkIdentity != 0 || got.hostIPv6.IsValid() {
		t.Fatalf("unexpected deny-by-default workload prefix: %+v", got)
	}
	if got := identities[1]; got.prefix.String() != "fd00:200:1::10/128" || got.networkIdentity != 9 || got.hostIPv6.String() != "2001:db8::2" {
		t.Fatalf("unexpected known workload identity: %+v", got)
	}
}
