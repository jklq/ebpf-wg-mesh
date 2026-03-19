//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

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
				if _, err := store.upsertAgent(ctx, testAgentHello(i)); err != nil {
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
					if got, want := len(peer.GetAllowedIps()), 1; got != want {
						t.Fatalf("agent %s peer %s expected %d allowed IP, got %d", agentID, peer.GetAgentId(), want, got)
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

func TestDesiredStateForSingleNodeClusterHasNoPeers(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.upsertAgent(ctx, testAgentHello(1)); err != nil {
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
	}
}
