package mesh

import (
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
)

// Assignment is the per-node mesh topology handed down by the control plane:
// the addresses this node answers for and the peers it tunnels to.
type Assignment struct {
	WorkloadIPv4Subnet string
	WorkloadIPv4Pool   string
	WorkloadIPv6Subnet string
	WorkloadIPv6Pool   string
	WireGuardAddresses []string
	Peers              []config.PeerConfig
	IdentitySeeds      []config.IdentitySeed
}

// AssignmentFrom reads the control plane's node config into an Assignment.
// A nil assigned config yields the zero Assignment, i.e. a node with no peers.
func AssignmentFrom(assigned *agentv1.AssignedNodeConfig) Assignment {
	assignment := Assignment{
		WorkloadIPv4Subnet: assigned.GetWorkloadIpv4Subnet(),
		WorkloadIPv4Pool:   assigned.GetWorkloadIpv4Pool(),
		WorkloadIPv6Subnet: assigned.GetWorkloadIpv6Subnet(),
		WorkloadIPv6Pool:   assigned.GetWorkloadIpv6Pool(),
		WireGuardAddresses: append([]string(nil), assigned.GetWireguardAddresses()...),
	}
	for _, identity := range assigned.GetWorkloadIdentities() {
		assignment.IdentitySeeds = append(assignment.IdentitySeeds, config.IdentitySeed{
			IPv4:            identity.GetWorkloadIpv4(),
			IPv6:            identity.GetWorkloadIpv6(),
			HostIPv6:        identity.GetHostIpv6(),
			NetworkIdentity: identity.GetNetworkIdentity(),
		})
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
	containerd := cfg.Containerd
	containerd.IdentitySeeds = append(append([]config.IdentitySeed(nil), cfg.Containerd.IdentitySeeds...), assignment.IdentitySeeds...)
	return config.MeshRuntimeConfig{
		NodeName:             cfg.Node.Name,
		Host:                 cfg.Mesh.Host,
		Containerd:           containerd,
		WireGuard:            wireGuard,
		Firewall:             cfg.Mesh.Firewall,
		WorkloadPoolCIDR:     assignment.WorkloadIPv6Pool,
		WorkloadIPv4PoolCIDR: assignment.WorkloadIPv4Pool,
	}
}
