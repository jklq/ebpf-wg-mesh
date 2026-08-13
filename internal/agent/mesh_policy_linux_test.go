//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/firewall"
	"ebof-wg-mesh/internal/testutil"

	"github.com/vishvananda/netlink"
)

func TestMeshPolicyAllowDenyUnknown(t *testing.T) {
	harness := startMeshPolicyHarness(t, true)
	a := harness.workload("a")
	waitForMeshHTTP(t, a.netns, harness.workload("b").ip, harness.workload("b").marker)
	assertMeshUnreachable(t, a.netns, harness.workload("c").ip)
	assertMeshUnreachable(t, a.netns, harness.unknownIP)
}

func TestMeshPolicyCatalogShrinkUpdatesLivePolicy(t *testing.T) {
	harness := startMeshPolicyHarness(t, true)
	a := harness.workload("a")
	b := harness.workload("b")
	waitForMeshHTTP(t, a.netns, b.ip, b.marker)

	shrunk := harness.cfg
	var remaining []config.IdentitySeed
	for _, seed := range harness.cfg.Containerd.IdentitySeeds {
		if seed.IPv6 == b.ip {
			continue
		}
		remaining = append(remaining, seed)
	}
	shrunk.Containerd.IdentitySeeds = remaining
	if err := harness.firewall.UpdateIdentityCatalog(shrunk); err != nil {
		t.Fatalf("UpdateIdentityCatalog(shrink): %v", err)
	}

	assertMeshUnreachable(t, a.netns, b.ip)
	assertMeshUnreachable(t, a.netns, harness.unknownIP)

	if err := harness.firewall.UpdateIdentityCatalog(harness.cfg); err != nil {
		t.Fatalf("UpdateIdentityCatalog(restore): %v", err)
	}
	waitForMeshHTTP(t, a.netns, b.ip, b.marker)
}

type meshPolicyHarness struct {
	cfg       config.MeshRuntimeConfig
	firewall  *firewall.Manager
	workloads map[string]meshWorkload
	unknownIP string
}

type meshWorkload struct {
	name   string
	alloc  string
	ip     string
	netns  string
	marker string
}

func (h *meshPolicyHarness) workload(name string) meshWorkload {
	return h.workloads[name]
}

func startMeshPolicyHarness(t *testing.T, includeC bool) *meshPolicyHarness {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("mesh policy tests require a privileged Linux host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	engine, agentCfg := newTestContainerdEngine(t)
	iface := meshTestIfaceName()
	createDummyInterface(t, iface)

	idAB := uint32(21)
	idC := uint32(22)
	hostIP := "fd00:44::10"
	suffix := uniqueRuntimeID("m")
	specs := []struct {
		name     string
		ip       string
		identity uint32
	}{
		{name: "a", ip: "fd00:200:8::a", identity: idAB},
		{name: "b", ip: "fd00:200:8::b", identity: idAB},
	}
	if includeC {
		specs = append(specs, struct {
			name     string
			ip       string
			identity uint32
		}{name: "c", ip: "fd00:200:8::c", identity: idC})
	}

	seeds := make([]config.IdentitySeed, 0, len(specs))
	for _, spec := range specs {
		seeds = append(seeds, config.IdentitySeed{
			IPv6:            spec.ip,
			HostIPv6:        hostIP,
			NetworkIdentity: spec.identity,
		})
	}
	meshCfg := config.MeshRuntimeConfig{
		NodeName: "mesh-policy-test",
		Host:     config.HostConfig{IPv6: hostIP},
		Containerd: config.ContainerdConfig{
			Socket:        agentCfg.Containerd.Socket,
			Namespace:     agentCfg.Containerd.Namespace,
			IdentitySeeds: seeds,
		},
		WireGuard:        config.WireGuard{InterfaceName: iface, ListenPort: 51821},
		Firewall:         config.FirewallConfig{ConntrackInnerEntries: 1024, MaxContainers: 32, ClusterIdentityEntries: 1024},
		WorkloadPoolCIDR: "fd00:200::/48",
	}

	fw, err := firewall.Start(ctx, meshCfg)
	if err != nil {
		t.Skipf("BPF/TCX cannot load on this host: %v", err)
	}
	t.Cleanup(func() {
		if err := fw.Close(); err != nil {
			t.Errorf("close firewall: %v", err)
		}
	})

	workloads := make(map[string]meshWorkload, len(specs))
	for _, spec := range specs {
		alloc := uniqueRuntimeID(spec.name)
		marker := spec.name + "-" + suffix
		svc := busyboxHTTPService(alloc, spec.identity, 1, spec.ip, marker)
		cleanupContainerdService(t, engine, agentCfg, alloc)
		if _, _, err := engine.EnsureService(ctx, svc); err != nil {
			if skippableRuntimeErr(err) {
				t.Skipf("containerd/CNI cannot start mesh workload: %v", err)
			}
			t.Fatalf("EnsureService(%s): %v", spec.name, err)
		}
		netns := requirePersistedNetNS(t, engine, alloc)
		waitForWorkloadMarker(t, ctx, netns, spec.ip, marker)
		workloads[spec.name] = meshWorkload{
			name:   spec.name,
			alloc:  alloc,
			ip:     spec.ip,
			netns:  netns,
			marker: marker,
		}
	}

	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 15 * time.Second, Interval: 50 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		return fw.AttachedCount() == len(workloads), nil
	}); err != nil {
		t.Fatalf("firewall did not attach to all workloads: attached=%d want=%d", fw.AttachedCount(), len(workloads))
	}

	return &meshPolicyHarness{
		cfg:       meshCfg,
		firewall:  fw,
		workloads: workloads,
		unknownIP: "fd00:200:8::d",
	}
}

func waitForMeshHTTP(t *testing.T, srcNetNS, dstIP, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 10 * time.Second, Interval: 100 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		body, err := httpGetInNamespaceErr(ctx, srcNetNS, dstIP, 8080, "/")
		return err == nil && body == want, nil
	}); err != nil {
		t.Fatalf("expected %s to serve %q from workload netns: %v", dstIP, want, err)
	}
}

func assertMeshUnreachable(t *testing.T, srcNetNS, dstIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	body, err := httpGetInNamespaceErr(ctx, srcNetNS, dstIP, 8080, "/")
	if err == nil {
		t.Fatalf("expected fail-closed deny to %s, got body %q", dstIP, body)
	}
}

func createDummyInterface(t *testing.T, name string) {
	t.Helper()
	if existing, err := netlink.LinkByName(name); err == nil {
		if err := netlink.LinkDel(existing); err != nil {
			t.Fatalf("delete existing %s: %v", name, err)
		}
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Skipf("cannot create dummy interface %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		_ = netlink.LinkDel(link)
		t.Skipf("cannot set %s up: %v", name, err)
	}
	t.Cleanup(func() {
		if current, err := netlink.LinkByName(name); err == nil {
			if err := netlink.LinkDel(current); err != nil {
				t.Errorf("delete dummy %s: %v", name, err)
			}
		}
	})
}

func meshTestIfaceName() string {
	return fmt.Sprintf("mp%x", time.Now().UnixNano()%0xffffff)
}
