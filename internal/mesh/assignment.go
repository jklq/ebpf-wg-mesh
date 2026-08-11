package mesh

import (
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
)

// Assignment is the per-node mesh topology handed down by the control plane:
// the addresses this node answers for and the peers it tunnels to.
type Assignment struct {
	WorkloadIPv6Subnet string
	WireGuardAddresses []string
	Peers              []config.PeerConfig
}

// AssignmentFrom reads the control plane's node config into an Assignment.
// A nil assigned config yields the zero Assignment, i.e. a node with no peers.
func AssignmentFrom(assigned *agentv1.AssignedNodeConfig) Assignment {
	assignment := Assignment{
		WorkloadIPv6Subnet: assigned.GetWorkloadIpv6Subnet(),
		WireGuardAddresses: append([]string(nil), assigned.GetWireguardAddresses()...),
	}
	for _, peer := range assigned.GetPeers() {
		assignment.Peers = append(assignment.Peers, config.PeerConfig{
			Name:                 peer.GetName(),
			PublicKey:            peer.GetPublicKey(),
			Endpoint:             peer.GetEndpoint(),
			AllowedIPs:           append([]string(nil), peer.GetAllowedIps()...),
			PersistentKeepaliveS: int(peer.GetPersistentKeepaliveSeconds()),
		})
	}
	return assignment
}

// RuntimeConfig folds an assignment into the node's static mesh config,
// producing the config Start consumes. The assignment owns the WireGuard
// addresses and peers; everything else comes from the node's own config.
func RuntimeConfig(cfg config.AgentConfig, assignment Assignment) config.MeshRuntimeConfig {
	wireGuard := cfg.Mesh.WireGuard
	wireGuard.Addresses = append([]string(nil), assignment.WireGuardAddresses...)
	wireGuard.Peers = append([]config.PeerConfig(nil), assignment.Peers...)
	return config.MeshRuntimeConfig{
		NodeName:   cfg.Node.Name,
		Host:       cfg.Mesh.Host,
		Containerd: cfg.Containerd,
		WireGuard:  wireGuard,
		Firewall:   cfg.Mesh.Firewall,
	}
}
