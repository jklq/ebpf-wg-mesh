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
		return strings.Compare(left.GetHostname(), right.GetHostname())
	})

	var out bytes.Buffer
	out.WriteString("127.0.0.1 localhost\n")
	out.WriteString("::1 localhost ip6-localhost ip6-loopback\n")
	for _, host := range sorted {
		hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host.GetHostname()), "."))
		ipv6, err := netip.ParseAddr(strings.TrimSpace(host.GetIpv6()))
		if err != nil || !ipv6.Is6() || hostname == "" {
			return nil, fmt.Errorf("invalid internal host %q at %q", hostname, host.GetIpv6())
		}
		shortName := strings.TrimSuffix(hostname, internalDomainSuffix)
		if shortName == hostname || strings.Contains(shortName, ".") {
			return nil, fmt.Errorf("invalid internal hostname %q", hostname)
		}
		fmt.Fprintf(&out, "%s %s %s\n", ipv6, hostname, shortName)
	}
	return out.Bytes(), nil
}
