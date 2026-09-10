package mesh

import (
	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
)

type Assignment struct {
	WorkloadIPv4Subnet string
	WorkloadIPv4Pool   string
	WorkloadIPv6Subnet string
	WorkloadIPv6Pool   string
	WireGuardAddresses []string
	Peers              []config.PeerConfig
	IdentitySeeds      []config.IdentitySeed
}

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
