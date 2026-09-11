package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalNetwork(t *testing.T) {
	network, gateway, err := localNetwork("192.168.77.0/24")
	if err != nil {
		t.Fatalf("localNetwork: %v", err)
	}
	if network.String() != "192.168.77.0/24" {
		t.Fatalf("network = %s", network.String())
	}
	if gateway.String() != "192.168.77.1" {
		t.Fatalf("gateway = %s", gateway.String())
	}
	if ip := localRoleIP(network, 0).String(); ip != "192.168.77.11" {
		t.Fatalf("controlplane ip = %s", ip)
	}
	if ip := localRoleIP(network, 1).String(); ip != "192.168.77.12" {
		t.Fatalf("agent-01 ip = %s", ip)
	}
}

func TestLocalNetworkRejectsInvalid(t *testing.T) {
	for _, subnet := range []string{"not-a-subnet", "fd00::/64", "10.0.0.0/30", "10.0.0.0/31", "127.0.0.0/24", "169.254.1.0/24", "203.0.113.0/24"} {
		if _, _, err := localNetwork(subnet); err == nil {
			t.Fatalf("localNetwork(%q) unexpectedly succeeded", subnet)
		}
	}
}

func TestLocalRoles(t *testing.T) {
	roles := localRoles(3)
	want := []string{"controlplane", "agent-01", "agent-02", "agent-03"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v want %v", roles, want)
	}
}

func TestLocalOptionsValidate(t *testing.T) {
	valid := localOptions{action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 4, cpMemMiB: 4096, agentVCPUs: 2, agentMemMiB: 2048, diskGiB: 20}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	cases := map[string]localOptions{
		"too few agents":   {action: "run", agents: 1, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8},
		"bridge too long":  {action: "run", agents: 2, bridge: "this-name-is-far-too-long", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8},
		"bad subnet":       {action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "10.0.0.0/30", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8},
		"unknown action":   {action: "frobnicate", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8},
		"memory too small": {action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 64, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8},
		"apt mirror ;":     {action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8, aptMirror: "http://example.com/ubuntu;id"},
		"apt mirror $()":   {action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8, aptMirror: "http://example.com/$(id)/ubuntu"},
		"apt mirror #":     {action: "run", agents: 2, bridge: "ebm1234abcd", subnet: "192.168.77.0/24", cpVCPUs: 1, cpMemMiB: 512, agentVCPUs: 1, agentMemMiB: 512, diskGiB: 8, aptMirror: "http://example.com/ubuntu#evil"},
	}
	for name, options := range cases {
		if err := options.validate(); err == nil {
			t.Fatalf("%s: validate unexpectedly succeeded", name)
		}
	}
}

func TestLocalRunNetworkIsRunSpecificAndHelperOwned(t *testing.T) {
	bridgeA, subnetA := localRunNetwork("vm-a")
	bridgeB, subnetB := localRunNetwork("vm-b")
	if !localBridgeNamePattern.MatchString(bridgeA) || bridgeA == bridgeB || subnetA == subnetB {
		t.Fatalf("derived networks collide or are unsafe: %q %q, %q %q", bridgeA, subnetA, bridgeB, subnetB)
	}
}

func TestLocalNetworkConfig(t *testing.T) {
	vm := localVMRecord{IP: "192.168.77.12", MAC: "52:54:00:6d:65:02"}
	config := localNetworkConfig(vm, "192.168.77.1", 24)
	for _, want := range []string{"macaddress: \"52:54:00:6d:65:02\"", "192.168.77.12/24", "via: 192.168.77.1", "dhcp4: false"} {
		if !strings.Contains(config, want) {
			t.Fatalf("network config missing %q:\n%s", want, config)
		}
	}
}

func TestPlanLocalVM(t *testing.T) {
	plan := localPlan{RunID: "vm-test", Subnet: "192.168.77.0/24"}
	vm, err := planLocalVM(plan, "agent-01", 1, "/tmp/run")
	if err != nil {
		t.Fatalf("planLocalVM: %v", err)
	}
	if vm.Name != "vm-test-agent-01" || vm.IP != "192.168.77.12" || vm.Tap != localTapName("vm-test", 1) || vm.MAC != "52:54:00:6d:65:02" {
		t.Fatalf("unexpected vm record: %+v", vm)
	}
	if filepath.Dir(vm.Overlay) != filepath.Join("/tmp/run", "agent-01") {
		t.Fatalf("overlay dir = %s", filepath.Dir(vm.Overlay))
	}
}

func TestLocalVMAuxiliaryPathsIgnoreEmptySeed(t *testing.T) {
	got := localVMAuxiliaryPaths(localVMRecord{}, true)
	if len(got) != 0 {
		t.Fatalf("paths = %v, want none", got)
	}
}

func TestQEMUOptionPathEscapesCommas(t *testing.T) {
	if got := qemuOptionPath("/tmp/run,one/disk.qcow2"); got != "/tmp/run,,one/disk.qcow2" {
		t.Fatalf("escaped path = %q", got)
	}
}

func TestLocalResourcesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-resources.json")
	state := localResources{
		RunID:  "vm-test",
		Bridge: "ebpfmesh0",
		Subnet: "192.168.77.0/24",
		VMs:    []localVMRecord{{Role: "controlplane", Name: "vm-test-controlplane", IP: "192.168.77.11", Tap: localTapName("vm-test", 0)}},
	}
	if err := writeLocalResources(path, state); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := readLocalResources(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if loaded.RunID != state.RunID || len(loaded.VMs) != 1 || loaded.VMs[0].Tap != state.VMs[0].Tap || loaded.Destroyed {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
}

func TestValidateAptMirrorRejectsShellMetacharacters(t *testing.T) {
	if err := validateAptMirror("http://archive.ubuntu.com/ubuntu"); err != nil {
		t.Fatalf("valid mirror rejected: %v", err)
	}
	for _, mirror := range []string{
		"http://example.com/ubuntu;id",
		"http://example.com/$(id)/ubuntu",
		"http://example.com/ubuntu#evil",
		"http://example.com/ubuntu && id",
		"not-a-url",
	} {
		if err := validateAptMirror(mirror); err == nil {
			t.Fatalf("accepted %q", mirror)
		}
	}
}

func TestDestroyLocalResourcesMissingManifestIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-local-resources.json")
	if err := destroyLocalResources(context.Background(), path, false); err != nil {
		t.Fatalf("missing manifest: %v", err)
	}
}

func TestDestroyLocalResourcesRemovesPreviouslyKeptDisks(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "disk.qcow2")
	seed := filepath.Join(dir, "seed.iso")
	console := filepath.Join(dir, "console.log")
	pidFile := filepath.Join(dir, "qemu.pid")
	paths := []string{overlay, seed, console, pidFile, filepath.Join(dir, "user-data"), filepath.Join(dir, "meta-data"), filepath.Join(dir, "network-config")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(dir, "local-resources.json")
	if err := writeLocalResources(manifest, localResources{Destroyed: true, VMs: []localVMRecord{{Overlay: overlay, Seed: seed, Console: console, PIDFile: pidFile}}}); err != nil {
		t.Fatal(err)
	}
	if err := destroyLocalResources(context.Background(), manifest, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("kept disk %s still exists: %v", path, err)
		}
	}
}

func TestParseLocalHelperUplink(t *testing.T) {
	got := parseLocalHelperUplink("bridge ebpfmesh0 up gateway=192.168.77.1/24 network=192.168.77.0/24 uplink=enp1s0")
	if got != "enp1s0" {
		t.Fatalf("uplink = %q", got)
	}
}

func TestLocalUserIsNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("localUser falls back to root when the process itself is root")
	}
	got := localUser()
	if got == "" {
		t.Fatal("localUser returned empty")
	}
	if got == "root" {
		t.Fatalf("localUser returned %q; expected a non-root user", got)
	}
}
