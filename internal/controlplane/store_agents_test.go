//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"fmt"
	"slices"
	"strings"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestRegisterAgentRejectsMissingWireGuardEndpoint(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	hello := agentHello("missing-endpoint")
	if err := enrollTestAgent(ctx, store, hello); err != nil {
		t.Fatal(err)
	}
	hello.WireguardEndpoint = ""
	if _, err := testDelivery(store).RegisterAgent(ctx, hello); err == nil || !strings.Contains(err.Error(), "wireguard_endpoint") {
		t.Fatalf("missing endpoint error = %v", err)
	}
}

func TestIPv4NodePrefixAllocationRejectsExhaustionAndOverlapTransactionally(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *persistence)
	}{
		{
			name: "exhaustion",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				for i, want := range []string{"10.42.0.0/30", "10.42.0.4/30"} {
					hello := testAgentHello(i + 1)
					if _, err := upsertTestAgent(t, store, ctx, hello); err != nil {
						t.Fatalf("upsertAgent(%s): %v", hello.GetAgentId(), err)
					}
					agent, err := store.reads.AgentByID(ctx, hello.GetAgentId())
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
				if _, err := registerAgent(ctx, store, hello); err == nil || !strings.Contains(err.Error(), "exhausted") {
					t.Fatalf("expected pool exhaustion, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 2)
			},
		},
		{
			name: "overlap",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := enrollTestAgent(ctx, store, testAgentHello(2)); err != nil {
					t.Fatal(err)
				}
				if err := store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `UPDATE agent_registrations SET workload_ipv4_subnet = '10.42.0.1/30' WHERE id = 'node-2'`); err != nil {
						return err
					}
					journal.RecordAgent(ctx, "node-2")
					return nil
				}); err != nil {
					t.Fatal(err)
				}

				hello := testAgentHello(3)
				if err := enrollTestAgent(ctx, store, hello); err != nil {
					t.Fatal(err)
				}
				if _, err := registerAgent(ctx, store, hello); err == nil || !strings.Contains(err.Error(), "overlap") {
					t.Fatalf("expected overlap rejection, got %v", err)
				}
				assertIPv4AllocatorUnchanged(t, store, "node-3", 1)
			},
		},
		{
			name: "workload address exhaustion",
			run: func(t *testing.T, store *persistence) {
				ctx := context.Background()
				if _, err := upsertTestAgent(t, store, ctx, testAgentHello(1)); err != nil {
					t.Fatal(err)
				}
				if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
					Users: []config.BootstrapUser{{ID: "user-1", Projects: []string{"demo"}}},
				}); err != nil {
					t.Fatal(err)
				}
				projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
				if err != nil || len(projects) != 1 {
					t.Fatalf("listProjects: projects=%d err=%v", len(projects), err)
				}
				environmentID := productionEnvironmentID(t, store, projects[0].ID)
				if _, err := createService(ctx, store, "user-1", environmentID, "first", serviceSpec(), "node-1"); err != nil {
					t.Fatal(err)
				}
				if _, err := createService(ctx, store, "user-1", environmentID, "second", serviceSpec(), "node-1"); err == nil || !strings.Contains(err.Error(), "exhausted") {
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
			store, err := openPersistence(config.DatabaseConfig{URL: createTestDatabase(t)}, meshCfg)
			if err != nil {
				t.Fatal(err)
			}
			delivery := newDelivery(store, nil, nil, nil, nil)
			if err := delivery.BecomeLive(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				delivery.ResignLive()
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			tc.run(t, store)
		})
	}
}

func assertIPv4AllocatorUnchanged(t *testing.T, store *persistence, unassignedAgent string, wantOrdinal int64) {
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

func TestAssignedNodeConfigSharedEnvironments(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "mesh")
	if err != nil {
		t.Fatal(err)
	}
	envA := productionEnvironmentID(t, store, project.ID)
	environmentB, err := store.catalog.createEnvironment(ctx, testUser("owner"), project.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := upsertTestAgent(t, store, ctx, testAgentHello(i)); err != nil {
			t.Fatal(err)
		}
	}
	add := func(env, name, agent string) deliverycore.ServiceRecord {
		t.Helper()
		service, err := createService(ctx, store, "owner", env, name, serviceSpec(), agent)
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	add(envA, "a", "node-1")
	add(environmentB.ID, "b", "node-2")
	add(environmentB.ID, "c", "node-3")
	check := func(agentID string, peers []string, identities int) {
		t.Helper()
		cfg, err := testDelivery(store).assignedNodeConfigForAgent(ctx, agentID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, peer := range cfg.GetPeers() {
			got = append(got, peer.GetAgentId())
			host, err := store.reads.AgentByID(ctx, peer.GetAgentId())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(peer.GetAllowedIps(), []string{host.WorkloadIPv4Subnet, host.WorkloadIPv6Subnet}) {
				t.Fatalf("peer prefixes = %v", peer.GetAllowedIps())
			}
		}
		if !slices.Equal(got, peers) || len(cfg.GetWorkloadIdentities()) != identities {
			t.Fatalf("%s peers=%v identities=%d, want %v/%d", agentID, got, len(cfg.GetWorkloadIdentities()), peers, identities)
		}
		if cfg.GetWorkloadIpv4Pool() == "" || cfg.GetWorkloadIpv6Pool() == "" {
			t.Fatal("missing fail-closed workload pools")
		}
	}
	check("node-1", nil, 1)
	check("node-2", []string{"node-3"}, 2)
	beforeA := mustDesiredRevision(t, store, ctx, "node-1")
	add(environmentB.ID, "b-growth", "node-3")
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != beforeA {
		t.Fatalf("disjoint environment growth bumped node-1: %d -> %d", beforeA, got)
	}
	check("node-1", nil, 1)
	shared := add(envA, "shared", "node-2")
	check("node-1", []string{"node-2"}, 2)
	check("node-2", []string{"node-1", "node-3"}, 5)
	check("node-3", []string{"node-2"}, 3)

	before := make(map[string]int64)
	for _, id := range []string{"node-1", "node-2", "node-3"} {
		before[id] = mustDesiredRevision(t, store, ctx, id)
	}
	hello := testAgentHello(1)
	hello.WireguardEndpoint = "192.0.2.99:51820"
	if _, err := registerAgent(ctx, store, hello); err != nil {
		t.Fatal(err)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-2"); got != before["node-2"]+1 {
		t.Fatal("endpoint change did not update shared peer")
	}
	if got := mustDesiredRevision(t, store, ctx, "node-3"); got != before["node-3"] {
		t.Fatal("endpoint change updated disjoint peer")
	}
	before["node-1"] = mustDesiredRevision(t, store, ctx, "node-1")
	before["node-2"] = mustDesiredRevision(t, store, ctx, "node-2")
	if err := deleteService(ctx, store, "owner", shared.ID); err != nil {
		t.Fatal(err)
	}
	check("node-1", nil, 1)
	check("node-2", []string{"node-3"}, 3)
	for _, id := range []string{"node-1", "node-2"} {
		if got := mustDesiredRevision(t, store, ctx, id); got != before[id]+1 {
			t.Fatalf("last shared allocation removal did not update %s", id)
		}
	}
	if got := mustDesiredRevision(t, store, ctx, "node-3"); got != before["node-3"] {
		t.Fatal("environment A removal updated environment B")
	}
}

func TestDesiredStateDistributesCrossNodeWorkloadIdentities(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"))
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
	services := make([]deliverycore.ServiceRecord, 0, 2)
	for i, agentID := range []string{"node-1", "node-2"} {
		service, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, projects[0].ID), fmt.Sprintf("web-%d", i+1), directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
			Ports: runtimePortsFromInts([]int32{8080}),
		}), agentID)
		if err != nil {
			t.Fatalf("createService(%s): %v", agentID, err)
		}
		services = append(services, service)
	}

	for _, agentID := range []string{"node-1", "node-2"} {
		state, err := desiredStateForAgent(ctx, store, agentID)
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
			t.Fatalf("agent %s expected 2 same-environment identities, got %d", agentID, len(identities))
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
	if err := deleteService(ctx, store, "user-1", services[0].ID); err != nil {
		t.Fatalf("deleteService: %v", err)
	}
	for _, item := range []struct {
		id     string
		before int64
	}{{"node-1", node1BeforeDelete}, {"node-2", node2BeforeDelete}} {
		if got := mustDesiredRevision(t, store, ctx, item.id); got != item.before+1 {
			t.Fatalf("agent %s identity revision was not bumped: got %d want %d", item.id, got, item.before+1)
		}
		state, err := desiredStateForAgent(ctx, store, item.id)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if item.id == "node-1" {
			want = 0
		}
		if got := len(state.GetNodeConfig().GetWorkloadIdentities()); got != want {
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

	state, err := desiredStateForAgent(ctx, store, "node-1")
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
		WireguardEndpoint:       fmt.Sprintf("192.0.2.%d:%d", 10+n, 51820+n),
		CpuMillisCapacity:       2000,
		MemoryMebibytesCapacity: 4096,
		RuntimeCapabilities:     []string{"containerd", "wireguard", "ebpf-policy"},
		SoftwareVersion:         "test",
	}
}
