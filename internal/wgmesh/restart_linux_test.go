//go:build linux

package wgmesh

import (
	"fmt"
	"os"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
)

func TestSetupAdoptsExistingWireGuardAndClosePreservesIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires privileged Linux")
	}
	name := fmt.Sprintf("wgr%x", time.Now().UnixNano()&0xffffff)
	t.Cleanup(func() {
		if err := Remove(name); err != nil {
			t.Error(err)
		}
	})
	cfg := config.WireGuard{InterfaceName: name, PrivateKey: testKey(9).String(), ListenPort: 51829,
		Addresses: []string{"fd00:44::9/128"}, Peers: []config.PeerConfig{
			testPeer("peer", testKey(2), "[::1]:51820", "fd00:44:2::/80", 5),
		}}
	r, err := Setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	iface, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	index := iface.Attrs().Index
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	iface, err = netlink.LinkByName(name)
	if err != nil || iface.Attrs().Index != index {
		t.Fatalf("Close destroyed interface: %v", err)
	}
	r2, err := Setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	iface, _ = netlink.LinkByName(name)
	if iface.Attrs().Index != index {
		t.Fatal("restart replaced interface")
	}
	client, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	device, err := client.Device(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(device.Peers) != 1 || device.PublicKey != r.state.privateKey.PublicKey() {
		t.Fatal("adoption lost device configuration")
	}
	// A stale assignment must reconcile in place, including removed routes and addresses.
	cfg.Addresses = []string{"fd00:44::10/128"}
	cfg.Peers = nil
	if err := r2.Update(cfg); err != nil {
		t.Fatal(err)
	}
	device, _ = client.Device(name)
	if len(device.Peers) != 0 {
		t.Fatal("stale peer retained")
	}
	addresses, err := netlink.AddrList(iface, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0].String() != cfg.Addresses[0] {
		t.Fatalf("addresses = %v", addresses)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Remove(name); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(name); err == nil {
		t.Fatal("explicit removal retained interface")
	}
}
