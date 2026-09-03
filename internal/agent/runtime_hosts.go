package agent

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

const internalDomainSuffix = ".mesh.internal"

func renderServiceHostsFile(hosts []*agentv1.InternalHost) ([]byte, error) {
	sorted := append([]*agentv1.InternalHost(nil), hosts...)
	slices.SortFunc(sorted, func(left, right *agentv1.InternalHost) int {
		if cmp := strings.Compare(left.GetHostname(), right.GetHostname()); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(left.GetIpv4(), right.GetIpv4()); cmp != 0 {
			return cmp
		}
		return strings.Compare(left.GetIpv6(), right.GetIpv6())
	})

	var out bytes.Buffer
	out.WriteString("127.0.0.1 localhost\n")
	out.WriteString("::1 localhost ip6-localhost ip6-loopback\n")
	for _, host := range sorted {
		hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host.GetHostname()), "."))
		ipv4, ipv4Err := netip.ParseAddr(strings.TrimSpace(host.GetIpv4()))
		ipv6, ipv6Err := netip.ParseAddr(strings.TrimSpace(host.GetIpv6()))
		if hostname == "" || (ipv4Err != nil && ipv6Err != nil) {
			return nil, fmt.Errorf("invalid internal host %q at %q/%q", hostname, host.GetIpv4(), host.GetIpv6())
		}
		if ipv4Err == nil && !ipv4.Is4() {
			return nil, fmt.Errorf("invalid internal host IPv4 %q", host.GetIpv4())
		}
		if ipv6Err == nil && !ipv6.Is6() {
			return nil, fmt.Errorf("invalid internal host IPv6 %q", host.GetIpv6())
		}
		shortName := strings.TrimSuffix(hostname, internalDomainSuffix)
		if shortName == hostname || strings.Contains(shortName, ".") {
			return nil, fmt.Errorf("invalid internal hostname %q", hostname)
		}
		if ipv4Err == nil {
			fmt.Fprintf(&out, "%s %s %s\n", ipv4, hostname, shortName)
		}
		if ipv6Err == nil {
			fmt.Fprintf(&out, "%s %s %s\n", ipv6, hostname, shortName)
		}
	}
	return out.Bytes(), nil
}
