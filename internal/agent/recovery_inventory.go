package agent

import (
	"encoding/json"
	"net/netip"
	"slices"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/recovery"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

var retainedNetworkKey = []byte("retained_recovery_network")

func retainRecoveryNetwork(tx *bbolt.Tx) error {
	meta := tx.Bucket(localMetaBucket)
	var reservations []recovery.NetworkReservation
	if raw := meta.Get(retainedNetworkKey); raw != nil {
		if err := json.Unmarshal(raw, &reservations); err != nil {
			return err
		}
	}
	if raw := tx.Bucket(localDesiredBucket).Get(desiredStateKey); raw != nil {
		var state agentv1.DesiredNodeState
		if err := proto.Unmarshal(raw, &state); err != nil {
			return err
		}
		if node := state.GetNodeConfig(); node != nil {
			for _, prefix := range []string{node.GetWorkloadIpv4Subnet(), node.GetWorkloadIpv6Subnet()} {
				if prefix != "" {
					reservations = append(reservations, recovery.NetworkReservation{Prefix: prefix})
				}
			}
			for _, prefix := range node.GetWireguardAddresses() {
				if p, err := netip.ParsePrefix(prefix); err == nil {
					reservations = append(reservations, recovery.NetworkReservation{Prefix: netip.PrefixFrom(p.Addr(), p.Addr().BitLen()).String()})
				}
			}
		}
		for _, service := range state.GetServices() {
			if service.GetNetworkIdentity() != 0 {
				reservations = append(reservations, recovery.NetworkReservation{Identity: service.GetNetworkIdentity(), EnvironmentID: service.GetEnvironmentId()})
			}
		}
	}
	slices.SortFunc(reservations, func(a, b recovery.NetworkReservation) int {
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		return slices.Compare(aa, bb)
	})
	encoded, err := json.Marshal(slices.Compact(reservations))
	if err != nil {
		return err
	}
	return meta.Put(retainedNetworkKey, encoded)
}

// fleetInventory contains no workload environment, pull credentials or secrets.
// Local metadata survives replacing desired state; live runtime labels repair
// it after local store loss and expose allocations created after the backup.
func (s *localStateStore) fleetInventory() (recovery.FleetHost, error) {
	var host recovery.FleetHost
	err := s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		host = recovery.FleetHost{ID: string(meta.Get(agentIdentityKey)), LocalStoreID: string(meta.Get(localStoreIDKey)), Generation: string(meta.Get(recoveryGenerationKey)), Reachable: true, AuthorityResolved: true}
		if raw := meta.Get(retainedNetworkKey); raw != nil {
			if err := json.Unmarshal(raw, &host.Reservations); err != nil {
				return err
			}
		}
		return tx.Bucket(localInventoryBucket).ForEach(func(_, raw []byte) error {
			var resource RuntimeResource
			if err := json.Unmarshal(raw, &resource); err != nil {
				return err
			}
			local, err := readAllocation(tx.Bucket(localAllocationsBucket), resource.AllocationID)
			if err != nil {
				return err
			}
			a := recovery.FleetAllocation{ID: resource.AllocationID, ServiceID: local.ServiceID, EnvironmentID: local.EnvironmentID, DeploymentID: local.DeploymentID, SpecRevision: local.DesiredSpecRevision, RolloutGeneration: local.DesiredGeneration, IPv4: local.IPv4, IPv6: local.IPv6, CreatedAt: local.CreatedAt}
			if resource.ServiceID != "" {
				a.ServiceID, a.EnvironmentID, a.DeploymentID = resource.ServiceID, resource.EnvironmentID, resource.DeploymentID
				a.SpecRevision, a.RolloutGeneration = resource.SpecRevision, resource.RolloutGeneration
				a.IPv4, a.IPv6 = resource.IPv4, resource.IPv6
			}
			if a.CreatedAt.IsZero() {
				a.CreatedAt = resource.CreatedAt
			}
			identity := resource.NetworkIdentity
			if identity == 0 {
				identity = local.NetworkIdentity
			}
			if identity != 0 {
				environment := a.EnvironmentID
				if environment == "" {
					environment = "unknown/" + a.ID
				}
				host.Reservations = append(host.Reservations, recovery.NetworkReservation{Identity: identity, EnvironmentID: environment})
			}
			host.Allocations = append(host.Allocations, a)
			return nil
		})
	})
	return host, err
}
