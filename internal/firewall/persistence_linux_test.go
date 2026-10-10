//go:build linux

package firewall

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/meshstate"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
)

func persistenceConfig(iface string) config.MeshRuntimeConfig {
	return config.MeshRuntimeConfig{
		WorkloadIPv4PoolCIDR: "10.200.0.0/16", WorkloadPoolCIDR: "fd00:200::/48",
		Host:      config.HostConfig{IPv6: "fd00:44::1"},
		WireGuard: config.WireGuard{InterfaceName: iface},
		Containerd: config.ContainerdConfig{Socket: "/run/containerd/containerd.sock", Namespace: "default", IdentitySeeds: []config.IdentitySeed{{
			IPv4: "10.200.1.10", IPv6: "fd00:200:1::10", HostIPv6: "fd00:44::1", NetworkIdentity: 9,
		}}},
		Firewall: config.FirewallConfig{MaxContainers: 1024, ConntrackInnerEntries: 10000, ClusterIdentityEntries: 65536},
	}
}

func TestFirewallPinsSurviveProcessExit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires privileged Linux and bpffs")
	}
	iface := fmt.Sprintf("fwp%x", time.Now().UnixNano()&0xffffff)
	workload := iface + "c"
	for _, name := range []string{iface, workload} {
		lnk := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(lnk); err != nil {
			t.Fatalf("create test interface: %v", err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(lnk) })
	}
	t.Cleanup(func() {
		if err := Remove(iface); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestFirewallPinProcessHelper$")
	cmd.Env = append(os.Environ(), "FIREWALL_PIN_HELPER="+iface)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pins-ready") {
		t.Fatalf("child did not create pins: %s", out)
	}
	cfg := persistenceConfig(iface)
	dir, _ := pinPath(iface)
	mapIDs := pinnedMapIDs(t, filepath.Join(dir, "maps"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := startPersistenceManager(ctx, cfg)
	if err != nil {
		t.Fatalf("adopt after process exit: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if m.AttachedCount() != 1 {
		t.Fatalf("adopted %d containers, want 1", m.AttachedCount())
	}
	r := m.containers["test-container"]
	var value uint64
	key := firewallConnectionKey{Protocol: 6, SrcPort: 1234, DstPort: 443}
	if err := r.innerMap.Lookup(key, &value); err != nil || value != 123456 {
		t.Fatalf("connection tracking lost: value=%d err=%v", value, err)
	}
	for name, id := range mapIDs {
		if got := pinnedMapIDs(t, filepath.Join(dir, "maps"))[name]; got != id {
			t.Fatalf("map %s replaced: %d -> %d", name, id, got)
		}
	}
	for _, ip := range []netip.Addr{r.ipv4, r.ipv6} {
		var identity firewallIdentityValue
		if err := m.objs.ClusterIdentityTrie.Lookup(identityKeyForPrefix(netip.PrefixFrom(ip, ip.BitLen())), &identity); err != nil {
			t.Fatal(err)
		}
		if identity.VethIfindex != r.ifindex {
			t.Fatalf("local identity overwritten on adoption: %+v", identity)
		}
	}
	before, _ := r.ingressLink.Info()
	if err := m.refreshContainerLinks(r); err != nil {
		t.Fatal(err)
	}
	after, _ := r.ingressLink.Info()
	if before.ID != after.ID {
		t.Fatal("reconciliation replaced the existing link")
	}
	if err := Remove(iface); err == nil {
		t.Fatal("removal allowed while manager owns network")
	}
	if err := m.handleTaskExit(r.containerID, "exec-id"); err != nil {
		t.Fatal(err)
	}
	if m.AttachedCount() != 1 {
		t.Fatal("exec exit removed workload")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	incompatible := cfg
	incompatible.Firewall.MaxContainers++
	if failed, err := startPersistenceManager(ctx, incompatible); err == nil {
		_ = failed.Close()
		t.Fatal("incompatible map layout was accepted")
	}
	for name, id := range mapIDs {
		if got := pinnedMapIDs(t, filepath.Join(dir, "maps"))[name]; got != id {
			t.Fatalf("failed startup replaced map %s", name)
		}
	}
	m2, err := startPersistenceManager(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	if err := m2.handleTaskExit(r.containerID, ""); err != nil {
		t.Fatal(err)
	}
	if m2.AttachedCount() != 0 {
		t.Fatal("workload exit retained attachment")
	}
	if _, err := os.Stat(m2.containerPinPath(r.containerID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workload pins retained: %v", err)
	}
	var policy firewallContainerPolicy
	if err := m2.objs.ContainerPolicyMap.Lookup(r.ifindex, &policy); !errors.Is(err, ebpf.ErrKeyNotExist) {
		t.Fatalf("policy retained on removal: %v", err)
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Remove(iface); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pins remain after removal: %v", err)
	}
}

func pinnedMapIDs(t *testing.T, dir string) map[string]ebpf.MapID {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]ebpf.MapID)
	for _, entry := range entries {
		m, err := ebpf.LoadPinnedMap(filepath.Join(dir, entry.Name()), nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := m.Info()
		_ = m.Close()
		if err != nil {
			t.Fatal(err)
		}
		id, ok := info.ID()
		if !ok {
			t.Fatal("map ID unavailable")
		}
		ids[entry.Name()] = id
	}
	return ids
}

func TestFirewallPinProcessHelper(t *testing.T) {
	iface := os.Getenv("FIREWALL_PIN_HELPER")
	if iface == "" {
		t.Skip("subprocess helper")
	}
	m, err := startPersistenceManager(context.Background(), persistenceConfig(iface))
	if err != nil {
		t.Fatal(err)
	}
	veth, err := netlink.LinkByName(iface + "c")
	if err != nil {
		t.Fatal(err)
	}
	ifindex := uint32(veth.Attrs().Index)
	inner, err := ebpf.NewMap(&ebpf.MapSpec{Name: fmt.Sprintf("ct_%d", ifindex), Type: ebpf.LRUHash, KeySize: 40, ValueSize: 8, MaxEntries: 10000})
	if err != nil {
		t.Fatal(err)
	}
	r := &containerRuntime{containerID: "test-container", ifindex: ifindex, networkIdentity: 9,
		ipv4: netip.MustParseAddr("10.200.1.10"), ipv6: netip.MustParseAddr("fd00:200:1::10"), innerMap: inner}
	if err := os.MkdirAll(m.containerPinPath(r.containerID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := inner.Pin(filepath.Join(m.containerPinPath(r.containerID), "conntrack")); err != nil {
		t.Fatal(err)
	}
	if err := inner.Put(firewallConnectionKey{Protocol: 6, SrcPort: 1234, DstPort: 443}, uint64(123456)); err != nil {
		t.Fatal(err)
	}
	if err := m.objs.ConntrackMatrix.Put(ifindex, inner); err != nil {
		t.Fatal(err)
	}
	if err := m.objs.ContainerPolicyMap.Put(ifindex, firewallContainerPolicy{NetworkIdentity: 9, Ipv4: r.ipv4.As4(), Ipv6: r.ipv6.As16()}); err != nil {
		t.Fatal(err)
	}
	if err := m.objs.InterfaceRoleMap.Put(ifindex, roleContainer); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.containers[r.containerID] = r
	if err := m.refreshContainerLinks(r); err != nil {
		t.Fatal(err)
	}
	m.mu.Unlock()
	fmt.Println("pins-ready")
	// No Close or defers: simulate abrupt agent death with live kernel handles.
	os.Exit(0)
}

func TestPinPathRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../wg0", "/wg0", "wg0/other"} {
		if _, err := pinPath(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func startPersistenceManager(ctx context.Context, cfg config.MeshRuntimeConfig) (*Manager, error) {
	ownership, err := meshstate.Acquire(cfg.WireGuard.InterfaceName)
	if err != nil {
		return nil, err
	}
	m, err := Start(ctx, cfg, ownership)
	if err != nil {
		_ = ownership.Close()
	}
	return m, err
}
