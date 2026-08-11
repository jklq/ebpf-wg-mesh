package mesh

import (
	"reflect"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
)

func TestAssignmentFromMapsPeers(t *testing.T) {
	t.Parallel()

	got := AssignmentFrom(&agentv1.AssignedNodeConfig{
		WorkloadIpv6Subnet: "fd00:44:1::/80",
		WireguardAddresses: []string{"fd00:44::1/128"},
		Peers: []*agentv1.WireGuardPeer{{
			Name:                       "node-b",
			PublicKey:                  "peer-public-key",
			Endpoint:                   "[2001:db8::2]:51820",
			AllowedIps:                 []string{"fd00:44:2::/80"},
			PersistentKeepaliveSeconds: 25,
		}},
	})

	want := Assignment{
		WorkloadIPv6Subnet: "fd00:44:1::/80",
		WireGuardAddresses: []string{"fd00:44::1/128"},
		Peers: []config.PeerConfig{{
			Name:                 "node-b",
			PublicKey:            "peer-public-key",
			Endpoint:             "[2001:db8::2]:51820",
			AllowedIPs:           []string{"fd00:44:2::/80"},
			PersistentKeepaliveS: 25,
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected assignment:\n got %+v\nwant %+v", got, want)
	}
}

func TestAssignmentFromNilIsEmpty(t *testing.T) {
	t.Parallel()

	if got := AssignmentFrom(nil); !reflect.DeepEqual(got, Assignment{}) {
		t.Fatalf("expected zero assignment, got %+v", got)
	}
}

func TestRuntimeConfigOverlaysAssignment(t *testing.T) {
	t.Parallel()

	cfg := config.AgentConfig{
		Node:       config.NodeConfig{Name: "node-a"},
		Containerd: config.ContainerdConfig{Namespace: "platform"},
		Mesh: config.MeshConfig{
			Host: config.HostConfig{IPv6: "2001:db8::1"},
			WireGuard: config.WireGuard{
				InterfaceName: "wg0",
				PrivateKey:    "private-key",
				ListenPort:    51820,
				Addresses:     []string{"fd00:44::stale/128"},
				Peers:         []config.PeerConfig{{Name: "stale"}},
			},
			Firewall: config.FirewallConfig{MaxContainers: 64},
		},
	}
	assignment := Assignment{
		WireGuardAddresses: []string{"fd00:44::1/128"},
		Peers:              []config.PeerConfig{{Name: "node-b", PublicKey: "peer-public-key"}},
	}

	got := RuntimeConfig(cfg, assignment)

	if got.NodeName != "node-a" || got.Host != cfg.Mesh.Host || got.Firewall != cfg.Mesh.Firewall {
		t.Fatalf("static node config not carried through: %+v", got)
	}
	if got.WireGuard.PrivateKey != "private-key" || got.WireGuard.InterfaceName != "wg0" {
		t.Fatalf("wireguard identity not carried through: %+v", got.WireGuard)
	}
	if !reflect.DeepEqual(got.WireGuard.Addresses, assignment.WireGuardAddresses) {
		t.Fatalf("assignment addresses not applied: %+v", got.WireGuard.Addresses)
	}
	if !reflect.DeepEqual(got.WireGuard.Peers, assignment.Peers) {
		t.Fatalf("assignment peers not applied: %+v", got.WireGuard.Peers)
	}

	// The caller's config must survive being overlaid repeatedly.
	if len(cfg.Mesh.WireGuard.Peers) != 1 || cfg.Mesh.WireGuard.Peers[0].Name != "stale" {
		t.Fatalf("RuntimeConfig mutated the source config: %+v", cfg.Mesh.WireGuard)
	}
}
