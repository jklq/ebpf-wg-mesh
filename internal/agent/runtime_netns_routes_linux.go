//go:build linux

package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// routeWorkloadNamespaceThroughGateway routes the workload pool via the
// namespace gateway. The CNI bridge assigns the whole pool prefix to the
// workload interface, which makes every peer node's pool range look on-link;
// without this the workload never consults its default route and cross-agent
// traffic never reaches the WireGuard interface.
func routeWorkloadNamespaceThroughGateway(namespacePath string) (err error) {
	namespacePath = strings.TrimSpace(namespacePath)
	if namespacePath == "" {
		return nil
	}
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("capture agent network namespace: %w", err)
	}
	defer hostNS.Close()
	targetNS, err := netns.GetFromPath(namespacePath)
	if err != nil {
		return fmt.Errorf("open workload network namespace: %w", err)
	}
	defer targetNS.Close()

	// netns.Set only affects the calling thread, so pin it for the whole swap.
	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()
	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("enter workload network namespace: %w", err)
	}
	defer func() {
		if restoreErr := netns.Set(hostNS); restoreErr != nil {
			unlockThread = false
			err = errors.Join(err, fmt.Errorf("restore host network namespace: %w", restoreErr))
		}
	}()

	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if err := routeFamilyThroughGateway(family); err != nil {
			return err
		}
	}
	return nil
}

func routeFamilyThroughGateway(family int) error {
	routes, err := netlink.RouteList(nil, family)
	if err != nil {
		return fmt.Errorf("list workload routes: %w", err)
	}
	var gateway net.IP
	var gatewayLinkIndex int
	var pool *netlink.Route
	for i := range routes {
		route := routes[i]
		if route.Table != 0 && route.Table != unix.RT_TABLE_MAIN {
			continue
		}
		if route.Type != 0 && route.Type != unix.RTN_UNICAST {
			continue
		}
		if route.Gw != nil && isDefaultDestination(route.Dst) {
			gateway = route.Gw
			gatewayLinkIndex = route.LinkIndex
			continue
		}
		if route.Dst == nil || isHostRoute(route.Dst) || route.Dst.IP.IsUnspecified() || route.Dst.IP.IsLoopback() || route.Dst.IP.IsMulticast() || route.Dst.IP.IsLinkLocalUnicast() || route.Dst.IP.IsLinkLocalMulticast() {
			continue
		}
		if route.Gw != nil {
			continue
		}
		if pool == nil || moreSpecific(route.Dst, pool.Dst) {
			pool = &routes[i]
		}
	}
	if gateway != nil && pool == nil && gatewayLinkIndex != 0 {
		link, linkErr := netlink.LinkByIndex(gatewayLinkIndex)
		if linkErr != nil {
			return fmt.Errorf("load workload gateway link: %w", linkErr)
		}
		addresses, addrErr := netlink.AddrList(link, family)
		if addrErr != nil {
			return fmt.Errorf("list workload addresses: %w", addrErr)
		}
		for _, address := range addresses {
			if address.IPNet != nil && !isHostRoute(address.IPNet) && !address.IPNet.IP.IsLinkLocalUnicast() && !address.IPNet.IP.IsLinkLocalMulticast() {
				// The assigned address carries host bits (for example 10.200.0.3/16);
				// kernel routes require the canonical network prefix.
				pool = &netlink.Route{LinkIndex: gatewayLinkIndex, Dst: canonicalPrefix(address.IPNet)}
				break
			}
		}
	}
	var poolDst *net.IPNet
	if pool != nil {
		poolDst = pool.Dst
	}
	poolLinkIndex := 0
	if pool != nil {
		poolLinkIndex = pool.LinkIndex
	}
	if err := decideWorkloadPoolRoute(gateway, poolDst, gatewayLinkIndex, poolLinkIndex); err != nil {
		return fmt.Errorf("%s: %w", routeFamilyName(family), err)
	}
	if gateway == nil || pool == nil {
		return nil
	}
	// Keep the gateway itself on-link so the replacement route cannot recurse.
	gatewayRoute := &netlink.Route{
		LinkIndex: pool.LinkIndex,
		Dst:       &net.IPNet{IP: gateway, Mask: fullAddressMask(gateway)},
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteAdd(gatewayRoute); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("pin workload gateway %s: %w", gateway, err)
	}
	replacement := &netlink.Route{LinkIndex: pool.LinkIndex, Dst: pool.Dst, Gw: gateway}
	if err := netlink.RouteReplace(replacement); err != nil {
		return fmt.Errorf("route workload pool %s via %s: %w", pool.Dst, gateway, err)
	}
	slog.Info("routed workload pool through gateway", "pool", pool.Dst.String(), "gateway", gateway.String())
	return nil
}

func isDefaultDestination(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0
}

func routeFamilyName(family int) string {
	if family == netlink.FAMILY_V6 {
		return "ipv6"
	}
	return "ipv4"
}

func isHostRoute(dst *net.IPNet) bool {
	ones, bits := dst.Mask.Size()
	return ones == bits
}

func moreSpecific(a, b *net.IPNet) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	aOnes, _ := a.Mask.Size()
	bOnes, _ := b.Mask.Size()
	return aOnes > bOnes
}

func fullAddressMask(ip net.IP) net.IPMask {
	if ip.To4() != nil {
		return net.CIDRMask(32, 32)
	}
	return net.CIDRMask(128, 128)
}
