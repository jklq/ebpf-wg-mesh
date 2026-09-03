//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestIPv4NodePrefixAllocationRejectsExhaustionAndOverlapTransactionally(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{
			name: "exhaustion",
			run: func(t *testing.T, store *Store) {
				ctx := context.Background()
				for i, want := range []string{"10.42.0.0/30", "10.42.0.4/30"} {
					hello := testAgentHello(i + 1)
					if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
						t.Fatalf("upsertAgent(%s): %v", hello.GetAgentId(), err)
					}
					agent, err := store.agentByID(ctx, hello.GetAgentId())
					if err != nil {
						t.Fatal(err)
					}
					if agent.WorkloadIPv4Subnet != want {
						t.Fatalf("agent %s prefix = %q, want %q", hello.GetAgentId(), agent.WorkloadIPv4Subnet, want)
					}
				}

				hello := testAgentHello(3)
				if err := enrollTestAgent(ctx, store, hello); err != nil {
					t.Fatal(err)
				}
				if _, err := store.upsertAgent(ctx, hello); err == nil || !strings.Contains(err.Error(), "exhausted") {
					t.Fatalf("expected pool exhaustion, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 2)
			},
		},
		{
			name: "overlap",
			run: func(t *testing.T, store *Store) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := enrollTestAgent(ctx, store, testAgentHello(2)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE agents SET workload_ipv4_subnet = '10.42.0.1/30' WHERE id = 'node-2'`); err != nil {
					t.Fatal(err)
				}

				hello := testAgentHello(3)
				if err := enrollTestAgent(ctx, store, hello); err != nil {
					t.Fatal(err)
				}
				if _, err := store.upsertAgent(ctx, hello); err == nil || !strings.Contains(err.Error(), "overlap") {
					t.Fatalf("expected overlap rejection, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 1)
			},
		},
		{
			name: "workload address exhaustion",
			run: func(t *testing.T, store *Store) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
					Users: []config.BootstrapUser{{ID: "user-1", Projects: []string{"demo"}}},
				}); err != nil {
					t.Fatal(err)
				}
				projects, err := store.listProjects(ctx, "user-1")
				if err != nil || len(projects) != 1 {
					t.Fatalf("listProjects: projects=%d err=%v", len(projects), err)
				}
				environmentID := productionEnvironmentID(t, store, projects[0].ID)
				if _, err := store.createService(ctx, "user-1", environmentID, "first", serviceSpec(), "node-1"); err != nil {
					t.Fatal(err)
				}
				if _, err := store.createService(ctx, "user-1", environmentID, "second", serviceSpec(), "node-1"); err == nil || !strings.Contains(err.Error(), "exhausted") {
					t.Fatalf("expected address exhaustion, got %v", err)
				}
				var services, allocations int
				if err := store.db.QueryRow(`SELECT count(*) FROM services`).Scan(&services); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow(`SELECT count(*) FROM allocations`).Scan(&allocations); err != nil {
					t.Fatal(err)
				}
				if services != 1 || allocations != 1 {
					t.Fatalf("exhausted allocation partially committed: services=%d allocations=%d", services, allocations)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meshCfg := testMeshConfig()
			meshCfg.WorkloadIPv4PoolCIDR = "10.42.0.0/29"
			meshCfg.WorkloadIPv4NodePrefixBits = 30
			store, err := OpenStore(config.DatabaseConfig{URL: createTestDatabase(t)}, meshCfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			tc.run(t, store)
		})
	}
}

func assertIPv4AllocatorUnchanged(t *testing.T, store *Store, unassignedAgent string, wantOrdinal int64) {
	t.Helper()
	var ordinal int64
	if err := store.db.QueryRow(`SELECT next_ordinal FROM workload_ipv4_prefix_allocator WHERE id = TRUE`).Scan(&ordinal); err != nil {
		t.Fatal(err)
	}
	if ordinal != wantOrdinal {
		t.Fatalf("allocator ordinal = %d, want %d after rejected transaction", ordinal, wantOrdinal)
	}
	var prefix string
	if err := store.db.QueryRow(`SELECT workload_ipv4_subnet FROM agents WHERE id = $1`, unassignedAgent).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if prefix != "" {
		t.Fatalf("rejected agent received prefix %q", prefix)
	}
}

func TestAssignedNodeConfigSupportsClusterSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		clusterSize int
	}{
		{name: "single agent", clusterSize: 1},
		{name: "two agents", clusterSize: 2},
		{name: "four agents", clusterSize: 4},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := openTestStore(t)
			ctx := context.Background()

			for i := 1; i <= tt.clusterSize; i++ {
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(i)); err != nil {
					t.Fatalf("upsertAgent(node-%d): %v", i, err)
				}
			}

			for i := 1; i <= tt.clusterSize; i++ {
				agentID := fmt.Sprintf("node-%d", i)
				cfg, err := store.assignedNodeConfigForAgent(ctx, agentID)
				if err != nil {
					t.Fatalf("assignedNodeConfigForAgent(%s): %v", agentID, err)
				}
				if got, want := len(cfg.GetPeers()), tt.clusterSize-1; got != want {
					t.Fatalf("agent %s expected %d peers, got %d", agentID, want, got)
				}
				if got, want := len(cfg.GetWireguardAddresses()), 1; got != want {
					t.Fatalf("agent %s expected %d wireguard address, got %d", agentID, want, got)
				}

				seen := make(map[string]struct{}, len(cfg.GetPeers()))
				for _, peer := range cfg.GetPeers() {
					if peer.GetAgentId() == agentID {
						t.Fatalf("agent %s unexpectedly included itself as a peer", agentID)
					}
					if peer.GetEndpoint() == "" {
						t.Fatalf("agent %s has peer %s with empty endpoint", agentID, peer.GetAgentId())
					}
					if got, want := len(peer.GetAllowedIps()), 2; got != want {
						t.Fatalf("agent %s peer %s expected %d allowed IP, got %d", agentID, peer.GetAgentId(), want, got)
					}
					families := map[bool]bool{}
					for _, raw := range peer.GetAllowedIps() {
						prefix, err := netip.ParsePrefix(raw)
						if err != nil {
							t.Fatalf("agent %s peer %s invalid AllowedIP %q: %v", agentID, peer.GetAgentId(), raw, err)
						}
						families[prefix.Addr().Is4()] = true
					}
					if !families[true] || !families[false] {
						t.Fatalf("agent %s peer %s AllowedIPs are not dual-stack: %v", agentID, peer.GetAgentId(), peer.GetAllowedIps())
					}
					seen[peer.GetAgentId()] = struct{}{}
				}
				if got, want := len(seen), tt.clusterSize-1; got != want {
					t.Fatalf("agent %s expected %d unique peers, got %d", agentID, want, got)
				}
			}
		})
	}
}

func TestDesiredStateDistributesCrossNodeWorkloadIdentities(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	for i, host := range []string{"fd00:30::10", "fd00:30::11"} {
		hello := testAgentHello(i + 1)
		hello.AdvertiseAddr = host
		if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
			t.Fatalf("upsertAgent: %v", err)
		}
	}
	services := make([]serviceRecord, 0, 2)
	for i, agentID := range []string{"node-1", "node-2"} {
		service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), fmt.Sprintf("web-%d", i+1), directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
			Ports: runtimePortsFromInts([]int32{8080}),
		}), agentID)
		if err != nil {
			t.Fatalf("createService(%s): %v", agentID, err)
		}
		services = append(services, service)
	}

	for _, agentID := range []string{"node-1", "node-2"} {
		state, err := store.desiredStateForAgent(ctx, agentID)
		if err != nil {
			t.Fatalf("desiredStateForAgent(%s): %v", agentID, err)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv6Pool(); got != "fd00:200::/48" {
			t.Fatalf("agent %s got workload pool %q", agentID, got)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv4Pool(); got != "10.200.0.0/16" {
			t.Fatalf("agent %s got IPv4 workload pool %q", agentID, got)
		}
		if got := state.GetNodeConfig().GetWorkloadIpv4Subnet(); got == "" {
			t.Fatalf("agent %s got empty IPv4 workload subnet", agentID)
		}
		identities := state.GetNodeConfig().GetWorkloadIdentities()
		if len(identities) != 2 {
			t.Fatalf("agent %s expected 2 cluster identities, got %d", agentID, len(identities))
		}
		seenHosts := map[string]string{}
		for _, identity := range identities {
			if identity.GetEnvironmentId() != services[0].EnvironmentID || identity.GetNetworkIdentity() == 0 ||
				identity.GetWorkloadIpv4() == "" || identity.GetWorkloadIpv6() == "" {
				t.Fatalf("agent %s got invalid tenant identity %+v", agentID, identity)
			}
			seenHosts[identity.GetHostAgentId()] = identity.GetHostIpv6()
		}
		if seenHosts["node-1"] != "fd00:30::10" || seenHosts["node-2"] != "fd00:30::11" {
			t.Fatalf("agent %s got incomplete host identities: %+v", agentID, seenHosts)
		}
	}

	node1BeforeDelete := mustDesiredRevision(t, store, ctx, "node-1")
	node2BeforeDelete := mustDesiredRevision(t, store, ctx, "node-2")
	if err := store.deleteService(ctx, "user-1", projects[0].ID, services[0].ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	for _, item := range []struct {
		id     string
		before int64
	}{{"node-1", node1BeforeDelete}, {"node-2", node2BeforeDelete}} {
		if got := mustDesiredRevision(t, store, ctx, item.id); got != item.before+1 {
			t.Fatalf("agent %s identity revision was not bumped: got %d want %d", item.id, got, item.before+1)
		}
		state, err := store.desiredStateForAgent(ctx, item.id)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(state.GetNodeConfig().GetWorkloadIdentities()); got != 1 {
			t.Fatalf("agent %s retained deleted workload identity, got %d", item.id, got)
		}
	}
}

func TestDesiredStateForSingleNodeClusterHasNoPeers(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
		t.Fatalf("upsertAgent(node-1): %v", err)
	}

	state, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent(node-1): %v", err)
	}
	if state.GetNodeConfig() == nil {
		t.Fatal("expected node config in desired state")
	}
	if got := len(state.GetNodeConfig().GetPeers()); got != 0 {
		t.Fatalf("expected no peers for single-node cluster, got %d", got)
	}
}

func testAgentHello(n int) *agentv1.AgentHello {
	id := fmt.Sprintf("node-%d", n)
	return &agentv1.AgentHello{
		AgentId:                 id,
		Name:                    id,
		AdvertiseAddr:           fmt.Sprintf("fd00:30::%x", 0x10+n),
		WireguardPublicKey:      fmt.Sprintf("test-public-key-%d", n),
		WireguardListenPort:     51820 + int32(n),
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
	}
}
