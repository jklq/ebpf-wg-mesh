//go:build linux

package agent

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestRouteWorkloadNamespaceThroughGateway verifies the pool prefix assigned by
// the CNI bridge is rerouted via the namespace gateway so peer node ranges are
// no longer treated as on-link.
func TestRouteWorkloadNamespaceThroughGateway(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create network namespaces")
	}
	name := fmt.Sprintf("route-test-%d", time.Now().UnixNano())
	// netns.NewNamed leaves the calling thread inside the new namespace, so pin
	// the goroutine to this thread and capture the host namespace first; the
	// thread must be restored before the namespace is deleted in cleanup.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer hostNS.Close()
	target, err := netns.NewNamed(name)
	if err != nil {
		t.Skipf("cannot create network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = target.Close()
		_ = netns.DeleteNamed(name)
	})
	path := "/var/run/netns/" + name

	// Build a netns that looks like a CNI bridge workload: an interface with an
	// address in the pool and a default route via the bridge gateway.
	setupErr := func() error {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth9"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
		addr, err := netlink.ParseAddr("10.200.0.3/16")
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return err
		}
		return netlink.RouteAdd(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &net.IPNet{IP: net.ParseIP("0.0.0.0"), Mask: net.CIDRMask(0, 32)},
			Gw:        net.ParseIP("10.200.0.1"),
		})
	}()
	_ = netns.Set(hostNS)
	if setupErr != nil {
		t.Fatalf("setup test namespace: %v", setupErr)
	}

	if err := routeWorkloadNamespaceThroughGateway(path); err != nil {
		t.Fatalf("routeWorkloadNamespaceThroughGateway: %v", err)
	}

	if err := netns.Set(target); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = netns.Set(hostNS) }()
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	routedViaGateway := false
	for _, route := range routes {
		if route.Dst == nil || route.Gw == nil || route.Dst.String() != "10.200.0.0/16" {
			continue
		}
		if route.Gw.String() == "10.200.0.1" {
			routedViaGateway = true
		}
	}
	if !routedViaGateway {
		t.Fatalf("workload pool was not routed via the gateway: %+v", routes)
	}

	if err := netns.Set(hostNS); err != nil {
		t.Fatal(err)
	}
	if err := routeWorkloadNamespaceThroughGateway(path); err != nil {
		t.Fatalf("idempotent rewrite: %v", err)
	}
}

func TestRouteFamilyThroughGatewayFailsClosedWhenPoolMissing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create network namespaces")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer hostNS.Close()
	name := fmt.Sprintf("route-incomplete-%d", time.Now().UnixNano())
	target, err := netns.NewNamed(name)
	if err != nil {
		t.Skipf("cannot create network namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = target.Close()
		_ = netns.DeleteNamed(name)
	})
	setupErr := func() error {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth9"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
		gateway := net.ParseIP("10.200.0.1")
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: gateway, Mask: net.CIDRMask(32, 32)}, Scope: netlink.SCOPE_LINK}); err != nil {
			return err
		}
		return netlink.RouteAdd(&netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &net.IPNet{IP: net.ParseIP("0.0.0.0"), Mask: net.CIDRMask(0, 32)},
			Gw:        net.ParseIP("10.200.0.1"),
		})
	}()
	_ = netns.Set(hostNS)
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}
	if err := routeWorkloadNamespaceThroughGateway("/var/run/netns/" + name); err == nil {
		t.Fatal("expected incomplete route family to fail closed")
	}
}
