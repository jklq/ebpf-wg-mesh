package meshlabels

import (
	"fmt"
	"net/netip"
	"strconv"
)

const (
	Managed                  = "platform.managed"
	AllocationID             = "platform.allocation_id"
	ServiceID                = "platform.service_id"
	DesiredSpecRevision      = "platform.desired_spec_revision"
	DesiredRolloutGeneration = "platform.desired_rollout_generation"
)

const (
	DefaultEnvironmentKey = "mesh.environment_id"
	DefaultIPv4Key        = "mesh.ipv4"
	DefaultIPv6Key        = "mesh.ipv6"
)

type Identity struct {
	NetworkIdentity uint32
	IPv4            netip.Addr
	IPv6            netip.Addr
}

type Keys struct {
	Environment string
	IPv4        string
	IPv6        string
}

func NewKeys(environmentKey, ipv4Key, ipv6Key string) Keys {
	if environmentKey == "" {
		environmentKey = DefaultEnvironmentKey
	}
	if ipv4Key == "" {
		ipv4Key = DefaultIPv4Key
	}
	if ipv6Key == "" {
		ipv6Key = DefaultIPv6Key
	}
	return Keys{Environment: environmentKey, IPv4: ipv4Key, IPv6: ipv6Key}
}

func (k Keys) Encode(id Identity) map[string]string {
	ipv4 := ""
	if id.IPv4.IsValid() {
		ipv4 = id.IPv4.String()
	}
	ipv6 := ""
	if id.IPv6.IsValid() {
		ipv6 = id.IPv6.String()
	}
	return map[string]string{
		k.Environment: strconv.FormatUint(uint64(id.NetworkIdentity), 10),
		k.IPv4:        ipv4,
		k.IPv6:        ipv6,
	}
}

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

	rawIPv4 := labels[k.IPv4]
	if rawIPv4 == "" {
		return Identity{}, fmt.Errorf("missing label %q", k.IPv4)
	}
	ipv4, err := netip.ParseAddr(rawIPv4)
	if err != nil || !ipv4.Is4() {
		return Identity{}, fmt.Errorf("invalid ipv4 label %q", rawIPv4)
	}

	rawIP := labels[k.IPv6]
	if rawIP == "" {
		return Identity{}, fmt.Errorf("missing label %q", k.IPv6)
	}
	ip, err := netip.ParseAddr(rawIP)
	if err != nil || !ip.Is6() {
		return Identity{}, fmt.Errorf("invalid ipv6 label %q", rawIP)
	}

	return Identity{NetworkIdentity: uint32(networkIdentity), IPv4: ipv4, IPv6: ip}, nil
}

func (k Keys) NetworkIdentity(labels map[string]string) uint32 {
	value, _ := strconv.ParseUint(labels[k.Environment], 10, 32)
	return uint32(value)
}
