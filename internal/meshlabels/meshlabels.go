// Package meshlabels defines the container labels the agent stamps onto managed
// workloads and the firewall reads back to recover their mesh identity. Both
// sides must agree on the key names and the value encoding, so neither writes
// them by hand.
package meshlabels

import (
	"fmt"
	"net/netip"
	"strconv"
)

// Bookkeeping labels present on every workload the platform manages.
const (
	Managed                  = "platform.managed"
	AllocationID             = "platform.allocation_id"
	ServiceID                = "platform.service_id"
	DesiredSpecRevision      = "platform.desired_spec_revision"
	DesiredRolloutGeneration = "platform.desired_rollout_generation"
)

// Default keys for the identity labels, which operators may rename.
const (
	DefaultEnvironmentKey = "mesh.environment_id"
	DefaultIPv6Key        = "mesh.ipv6"
)

// Identity is a workload's mesh identity: the network identity its datapath
// policy is keyed by, and the address it owns inside the mesh.
type Identity struct {
	NetworkIdentity uint32
	IPv6            netip.Addr
}

// Keys names the labels carrying an Identity. Because they are configurable,
// the writing and reading sides must be handed the same pair.
type Keys struct {
	Environment string
	IPv6        string
}

// NewKeys resolves configured label keys, substituting the defaults for empty
// ones so a partially configured agent and firewall still agree.
func NewKeys(environmentKey, ipv6Key string) Keys {
	if environmentKey == "" {
		environmentKey = DefaultEnvironmentKey
	}
	if ipv6Key == "" {
		ipv6Key = DefaultIPv6Key
	}
	return Keys{Environment: environmentKey, IPv6: ipv6Key}
}

// Encode renders id into the label pair named by k.
func (k Keys) Encode(id Identity) map[string]string {
	ipv6 := ""
	if id.IPv6.IsValid() {
		ipv6 = id.IPv6.String()
	}
	return map[string]string{
		k.Environment: strconv.FormatUint(uint64(id.NetworkIdentity), 10),
		k.IPv6:        ipv6,
	}
}

// Decode recovers the identity Encode wrote, rejecting missing or malformed
// values rather than attaching a workload to the wrong environment.
func (k Keys) Decode(labels map[string]string) (Identity, error) {
	if labels == nil {
		return Identity{}, fmt.Errorf("missing label %q", k.Environment)
	}

	rawEnvironment := labels[k.Environment]
	if rawEnvironment == "" {
		return Identity{}, fmt.Errorf("missing label %q", k.Environment)
	}
	networkIdentity, err := strconv.ParseUint(rawEnvironment, 10, 32)
	if err != nil || networkIdentity == 0 {
		return Identity{}, fmt.Errorf("invalid environment label %q", rawEnvironment)
	}

	rawIP := labels[k.IPv6]
	if rawIP == "" {
		return Identity{}, fmt.Errorf("missing label %q", k.IPv6)
	}
	ip, err := netip.ParseAddr(rawIP)
	if err != nil || !ip.Is6() {
		return Identity{}, fmt.Errorf("invalid ipv6 label %q", rawIP)
	}

	return Identity{NetworkIdentity: uint32(networkIdentity), IPv6: ip}, nil
}

// NetworkIdentity reports the network identity stamped under k.Environment, or zero when
// the label is absent or malformed. Callers comparing against a desired
// identity use this to avoid treating a decode failure as a mismatch reason.
func (k Keys) NetworkIdentity(labels map[string]string) uint32 {
	value, _ := strconv.ParseUint(labels[k.Environment], 10, 32)
	return uint32(value)
}
