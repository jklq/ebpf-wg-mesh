package controlplane

import (
	"crypto/sha1"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func ts(v time.Time) *timestamppb.Timestamp {
	return timestamppb.New(v)
}

func privateIPv6(subnetCIDR, projectID, serviceID string) (string, error) {
	prefix, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return "", fmt.Errorf("parse workload subnet %q: %w", subnetCIDR, err)
	}
	if !prefix.Addr().Is6() {
		return "", fmt.Errorf("workload subnet %q is not IPv6", subnetCIDR)
	}
	if bits := prefix.Bits(); bits != 64 {
		return "", fmt.Errorf("workload subnet %q must be /64", subnetCIDR)
	}
	base := prefix.Masked().Addr().As16()
	sum := sha1.Sum([]byte(projectID + ":" + serviceID))
	copy(base[8:], sum[:8])
	base[15] = 0x10
	return netip.AddrFrom16(base).String(), nil
}

func nextSubnetFromPool(poolCIDR string, prefixBits int, used map[string]struct{}, agentID string) (string, error) {
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool %q: %w", poolCIDR, err)
	}
	if !pool.Addr().Is6() {
		return "", fmt.Errorf("pool %q is not IPv6", poolCIDR)
	}
	if prefixBits < pool.Bits() || prefixBits > 128 {
		return "", fmt.Errorf("invalid child prefix /%d for pool %q", prefixBits, poolCIDR)
	}

	base := pool.Masked().Addr().As16()
	max := 1 << (prefixBits - pool.Bits())
	start := preferredOrdinal(agentID, max-1)
	for offset := 0; offset < max-1; offset++ {
		i := ((start + offset - 1) % (max - 1)) + 1
		addr := base
		writeSubnetBits(addr[:], pool.Bits(), prefixBits, i)
		prefix := netip.PrefixFrom(netip.AddrFrom16(addr), prefixBits).Masked().String()
		if _, exists := used[prefix]; !exists {
			return prefix, nil
		}
	}
	return "", fmt.Errorf("pool %q exhausted", poolCIDR)
}

func nextAddressFromPool(poolCIDR string, used map[string]struct{}, hostOffset uint16, agentID string) (string, error) {
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool %q: %w", poolCIDR, err)
	}
	if !pool.Addr().Is6() {
		return "", fmt.Errorf("pool %q is not IPv6", poolCIDR)
	}
	if pool.Bits() > 112 {
		return "", fmt.Errorf("pool %q too small for sequential node addresses", poolCIDR)
	}

	base := pool.Masked().Addr().As16()
	start := uint16(preferredOrdinal(agentID, 65535-int(hostOffset)-1))
	for offset := uint16(0); offset < 65535-hostOffset-1; offset++ {
		i := ((start + offset - 1) % (65535 - hostOffset - 1)) + 1
		addr := base
		binaryBigPutUint16(addr[14:], hostOffset+i)
		cidr := netip.PrefixFrom(netip.AddrFrom16(addr), pool.Bits()).String()
		if _, exists := used[cidr]; !exists {
			return cidr, nil
		}
	}
	return "", fmt.Errorf("pool %q exhausted", poolCIDR)
}

func writeSubnetBits(dst []byte, startBits, endBits, value int) {
	for bit := startBits; bit < endBits; bit++ {
		byteIndex := bit / 8
		bitIndex := 7 - (bit % 8)
		mask := byte(1 << bitIndex)
		if value&(1<<(endBits-bit-1)) != 0 {
			dst[byteIndex] |= mask
			continue
		}
		dst[byteIndex] &^= mask
	}
}

func binaryBigPutUint16(dst []byte, value uint16) {
	if len(dst) < 2 {
		return
	}
	dst[0] = byte(value >> 8)
	dst[1] = byte(value)
}

func endpointForAgent(advertiseAddr string, port int) (string, error) {
	if ip := net.ParseIP(advertiseAddr); ip == nil || ip.To16() == nil || ip.To4() != nil {
		return "", fmt.Errorf("advertise address must be IPv6: %q", advertiseAddr)
	}
	return fmt.Sprintf("[%s]:%d", advertiseAddr, port), nil
}

func preferredOrdinal(agentID string, max int) int {
	if max <= 0 {
		return 1
	}
	if suffix := trailingNumber(agentID); suffix > 0 {
		return ((suffix - 1) % max) + 1
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(agentID))
	return int(hasher.Sum32()%uint32(max)) + 1
}

func trailingNumber(value string) int {
	end := len(value)
	start := end
	for start > 0 {
		if value[start-1] < '0' || value[start-1] > '9' {
			break
		}
		start--
	}
	if start == end {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSpace(value[start:end]))
	if err != nil {
		return 0
	}
	return number
}
