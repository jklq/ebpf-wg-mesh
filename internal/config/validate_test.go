package config

import "testing"

func TestFinalizeControlPlaneAppliesDefaults(t *testing.T) {
	cfg := ControlPlaneConfig{
		Database: DatabaseConfig{
			URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		},
		InternalGRPC: ListenerConfig{
			TLS: ServerTLSConfig{
				BootstrapTokens: []string{"token-a"},
			},
		},
		Dashboard: ManagedDashboardConfig{
			Enabled: false,
		},
	}

	if err := FinalizeControlPlane(&cfg); err != nil {
		t.Fatalf("FinalizeControlPlane: %v", err)
	}
	if cfg.PublicHTTP.Listen != "0.0.0.0:8080" {
		t.Fatalf("unexpected public listen %q", cfg.PublicHTTP.Listen)
	}
	if got := cfg.Mesh.NetworkCIDR; got != "fd00:44::/64" {
		t.Fatalf("unexpected mesh network cidr %q", got)
	}
	if len(cfg.InternalGRPC.TLS.ServerNames) == 0 {
		t.Fatal("expected default internal server names")
	}
}

func TestFinalizeControlPlaneValidatesDashboardConfig(t *testing.T) {
	cfg := ControlPlaneConfig{
		PublicHTTP: ListenerConfig{Listen: "127.0.0.1:8080"},
		InternalGRPC: ListenerConfig{
			Listen: "127.0.0.1:9443",
			TLS: ServerTLSConfig{
				ServerNames:             []string{"controlplane"},
				BootstrapTokens:         []string{"token-a"},
				ServerCertValidityHours: 24,
				ClientCertValidityHours: 24,
			},
		},
		Database: DatabaseConfig{
			URL: "postgresql://root@127.0.0.1:26257/defaultdb?sslmode=disable",
		},
		StateDir: "var/controlplane",
		Ingress: IngressConfig{
			PublicAddr: "platform.example.test",
		},
		Dashboard: ManagedDashboardConfig{
			Enabled: true,
		},
		Mesh: ControlPlaneMeshConfig{
			InterfaceName:    "wg0",
			ListenPort:       51820,
			NetworkCIDR:      "fd00:44::/64",
			WorkloadPoolCIDR: "fd00:200::/48",
		},
	}

	err := FinalizeControlPlane(&cfg)
	if err == nil {
		t.Fatal("expected dashboard validation error")
	}
	if got := err.Error(); got != "controlplane.dashboard.image is required when dashboard is enabled" {
		t.Fatalf("unexpected error %q", got)
	}
}

func TestFinalizeAgentRejectsInvalidWireGuardPeerEndpoint(t *testing.T) {
	cfg := AgentConfig{
		Node: NodeConfig{
			ID:            "node-1",
			Name:          "node-1",
			AdvertiseAddr: "fd00:30::10",
		},
		ControlPlane: ControlPlaneClientConfig{
			Address: "controlplane:9443",
			TLS: ClientTLSConfig{
				CAFile:             "ca.crt",
				ServerName:         "controlplane",
				BootstrapToken:     "token-a",
				RenewBeforeMinutes: 30,
			},
		},
		Mesh: MeshConfig{
			Host: HostConfig{IPv6: "fd00:30::10"},
			WireGuard: WireGuard{
				ListenPort: 51820,
				Peers: []PeerConfig{{
					Name:       "node-2",
					PublicKey:  "pubkey",
					Endpoint:   "127.0.0.1:51820",
					AllowedIPs: []string{"fd00:44::/128"},
				}},
			},
		},
	}

	err := FinalizeAgent(&cfg)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got := err.Error(); got != `agent.mesh.wireguard peer "node-2" invalid endpoint: address 127.0.0.1: no such host` &&
		got != `agent.mesh.wireguard peer "node-2" invalid endpoint: host must be IPv6: "127.0.0.1"` {
		t.Fatalf("unexpected error %q", got)
	}
}
