package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ebof-wg-mesh/internal/testutil"
)

const (
	localHelperPath      = "/usr/local/bin/ebpf-wg-mesh-localnet"
	localDefaultCacheDir = ".cache/ebpf-wg-mesh"
	localFirstHostOctet  = 11
	localMACPrefix       = "52:54:00:6d:65"
)

type localOptions struct {
	action, bridge, subnet, baseImage, cacheDir, manifest, aptMirror string
	agents, cpVCPUs, cpMemMiB, agentVCPUs, agentMemMiB               int
	diskGiB                                                          int
	keepDisks                                                        bool
}

func registerLocalFlags() *localOptions {
	o := &localOptions{}
	flag.StringVar(&o.action, "local-action", "run", "local: run or destroy")
	flag.IntVar(&o.agents, "local-agents", 2, "number of local agent VMs (2..16)")
	flag.StringVar(&o.bridge, "local-bridge", "", "local VM bridge interface name (default: derived from run ID)")
	flag.StringVar(&o.subnet, "local-subnet", "", "local VM IPv4 subnet in CIDR form (default: derived from run ID)")
	flag.StringVar(&o.baseImage, "local-base-image", "", "local Ubuntu cloud image path (default: cached download)")
	flag.StringVar(&o.cacheDir, "local-cache-dir", "", "local VM base-image cache directory (default: ~/.cache/ebpf-wg-mesh)")
	flag.IntVar(&o.cpVCPUs, "local-cp-vcpus", 4, "controlplane VM vCPUs")
	flag.IntVar(&o.cpMemMiB, "local-cp-mem-mb", 6144, "controlplane VM memory in MiB")
	flag.IntVar(&o.agentVCPUs, "local-agent-vcpus", 2, "agent VM vCPUs")
	flag.IntVar(&o.agentMemMiB, "local-agent-mem-mb", 2048, "agent VM memory in MiB")
	flag.IntVar(&o.diskGiB, "local-disk-gb", 20, "local VM disk size in GiB (cloud-init grows the rootfs)")
	flag.StringVar(&o.manifest, "local-manifest", "", "existing local-resources.json to destroy")
	flag.BoolVar(&o.keepDisks, "local-keep-disks", false, "keep VM disk overlays after teardown")
	flag.StringVar(&o.aptMirror, "local-apt-mirror", "", "Ubuntu mirror for the prepared image (default: detected from the host's apt sources)")
	return o
}

var localBridgeNamePattern = regexp.MustCompile(`^ebm[0-9a-f]{8}$`)

func (o localOptions) validate() error {
	if o.agents < 2 || o.agents > 16 {
		return errors.New("local agents must be between 2 and 16")
	}
	if o.bridge != "" && !localBridgeNamePattern.MatchString(o.bridge) {
		return errors.New("local bridge name must match ebm[0-9a-f]{8}")
	}
	if o.subnet != "" {
		if _, _, err := localNetwork(o.subnet); err != nil {
			return err
		}
	}
	if o.action == "destroy" {
		return nil
	}
	for name, value := range map[string]int{"local-cp-vcpus": o.cpVCPUs, "local-agent-vcpus": o.agentVCPUs} {
		if value < 1 || value > 64 {
			return fmt.Errorf("%s must be between 1 and 64", name)
		}
	}
	for name, value := range map[string]int{"local-cp-mem-mb": o.cpMemMiB, "local-agent-mem-mb": o.agentMemMiB} {
		if value < 512 || value > 262144 {
			return fmt.Errorf("%s must be between 512 and 262144", name)
		}
	}
	if o.diskGiB < 8 || o.diskGiB > 1024 {
		return errors.New("local-disk-gb must be between 8 and 1024")
	}
	if o.action != "run" && o.action != "destroy" {
		return errors.New("local-action must be run or destroy")
	}
	if err := validateAptMirror(strings.TrimSpace(o.aptMirror)); err != nil {
		return err
	}
	return nil
}

var aptMirrorOK = regexp.MustCompile(`^https?://[A-Za-z0-9._:/=+-]+$`)

func validateAptMirror(mirror string) error {
	if mirror == "" {
		return nil
	}
	if !aptMirrorOK.MatchString(mirror) {
		return errors.New("local-apt-mirror must be an http(s) URL without whitespace, quotes, or shell metacharacters")
	}
	return nil
}

func localNetwork(subnet string) (*net.IPNet, net.IP, error) {
	_, network, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, nil, fmt.Errorf("parse local subnet: %w", err)
	}
	if network.IP.To4() == nil {
		return nil, nil, errors.New("local subnet must be IPv4")
	}
	if !network.IP.IsPrivate() || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() || network.IP.IsMulticast() {
		return nil, nil, errors.New("local subnet must be a private, non-local IPv4 network")
	}
	ones, bits := network.Mask.Size()
	if bits-ones < 8 {
		return nil, nil, errors.New("local subnet must have at least 8 host bits")
	}
	return network, localAddIP(network.IP, 1), nil
}

func localAddIP(ip net.IP, delta int) net.IP {
	base := ip.To4()
	if base == nil {
		return nil
	}
	out := make(net.IP, len(base))
	copy(out, base)
	value := int64(out[0])<<24 | int64(out[1])<<16 | int64(out[2])<<8 | int64(out[3])
	value += int64(delta)
	out[0] = byte(value >> 24)
	out[1] = byte(value >> 16)
	out[2] = byte(value >> 8)
	out[3] = byte(value)
	return out
}

func localRoleIP(network *net.IPNet, index int) net.IP {
	return localAddIP(network.IP, localFirstHostOctet+index)
}

func localRunNetwork(runID string) (string, string) {
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("ebm%x", sum[:4]), fmt.Sprintf("10.%d.%d.0/24", sum[4], sum[5])
}

// localIPv6Network derives a deterministic ULA prefix from the IPv4 subnet so
// each local VM has a stable IPv6 identity for the agent advertise address.
func localIPv6Network(subnet string) (*net.IPNet, net.IP, error) {
	_, network, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, nil, fmt.Errorf("parse local subnet: %w", err)
	}
	v4 := network.IP.To4()
	if v4 == nil {
		return nil, nil, errors.New("local subnet must be IPv4")
	}
	ula := make(net.IP, net.IPv6len)
	ula[0], ula[1] = 0xfd, 0x00
	copy(ula[2:6], v4)
	prefix := &net.IPNet{IP: ula, Mask: net.CIDRMask(64, 128)}
	return prefix, localAddIPv6(prefix.IP, 1), nil
}

func localAddIPv6(ip net.IP, delta int) net.IP {
	out := make(net.IP, net.IPv6len)
	copy(out, ip.To16())
	out[14] = byte(delta >> 8)
	out[15] = byte(delta)
	return out
}

func localRoleIPv6(network *net.IPNet, index int) net.IP {
	return localAddIPv6(network.IP, localFirstHostOctet+index)
}

var localHelperUserName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func localUser() string {
	if current, err := user.Current(); err == nil && current.Username != "" && current.Username != "root" {
		return current.Username
	}
	if value := strings.TrimSpace(os.Getenv("USER")); value != "" && value != "root" {
		return value
	}
	return "root"
}

func localTapOwner() (string, error) {
	name := localUser()
	if !localHelperUserName.MatchString(name) {
		return "", fmt.Errorf("local user %q is not a valid tap owner for %s (must match [a-z_][a-z0-9_-]{0,31})", name, localHelperPath)
	}
	return name, nil
}

func localCacheDir(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, localDefaultCacheDir), nil
}

func (o localOptions) baseImagePath() (string, error) {
	if strings.TrimSpace(o.baseImage) != "" {
		return o.baseImage, nil
	}
	cache, err := localCacheDir(o.cacheDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, localBaseImageName), nil
}

type localPlan struct {
	RunID              string `json:"run_id"`
	Bridge             string `json:"bridge"`
	Subnet             string `json:"subnet"`
	Gateway            string `json:"gateway"`
	IPv6Subnet         string `json:"ipv6_subnet"`
	BaseImage          string `json:"base_image"`
	BaseSHA            string `json:"base_sha"`
	Agents             int    `json:"agents"`
	Hosts              int    `json:"hosts"`
	ControlPlaneVCPUs  int    `json:"controlplane_vcpus"`
	ControlPlaneMemMiB int    `json:"controlplane_mem_mib"`
	AgentVCPUs         int    `json:"agent_vcpus"`
	AgentMemMiB        int    `json:"agent_mem_mib"`
	DiskGiB            int    `json:"disk_gib"`
	Timeout            string `json:"timeout"`
	AptMirror          string `json:"apt_mirror,omitempty"`
}

func localRoles(agents int) []string {
	roles := []string{"controlplane"}
	for i := 0; i < agents; i++ {
		roles = append(roles, fmt.Sprintf("agent-%02d", i+1))
	}
	return roles
}

func prepareLocal(ctx context.Context, o localOptions, runID string, timeout time.Duration) (localPlan, error) {
	var plan localPlan
	if err := o.validate(); err != nil {
		return plan, err
	}
	if !safeRunID.MatchString(runID) {
		return plan, errors.New("local run-id must match vm-[a-z0-9][a-z0-9-]{0,39}")
	}
	defaultBridge, defaultSubnet := localRunNetwork(runID)
	if o.bridge == "" {
		o.bridge = defaultBridge
	}
	if o.subnet == "" {
		o.subnet = defaultSubnet
	}
	if err := checkLocalPrereqs(); err != nil {
		return plan, err
	}
	baseImage, err := o.baseImagePath()
	if err != nil {
		return plan, err
	}
	network, gateway, err := localNetwork(o.subnet)
	if err != nil {
		return plan, err
	}
	ipv6Network, _, err := localIPv6Network(o.subnet)
	if err != nil {
		return plan, err
	}
	managed := strings.TrimSpace(o.baseImage) == ""
	baseSHA, err := ensureLocalBaseImage(ctx, baseImage, managed)
	if err != nil {
		return plan, err
	}
	ones, _ := network.Mask.Size()
	plan = localPlan{
		RunID:              runID,
		Bridge:             o.bridge,
		Subnet:             network.String(),
		Gateway:            fmt.Sprintf("%s/%d", gateway.String(), ones),
		IPv6Subnet:         ipv6Network.String(),
		BaseImage:          baseImage,
		BaseSHA:            baseSHA,
		Agents:             o.agents,
		Hosts:              o.agents + 1,
		ControlPlaneVCPUs:  o.cpVCPUs,
		ControlPlaneMemMiB: o.cpMemMiB,
		AgentVCPUs:         o.agentVCPUs,
		AgentMemMiB:        o.agentMemMiB,
		DiskGiB:            o.diskGiB,
		Timeout:            timeout.String(),
		AptMirror:          o.aptMirrorValue(),
	}
	return plan, nil
}

func checkLocalPrereqs() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return errors.New("local provider needs /dev/kvm; enable AMD-V/SVM (or VT-x) in BIOS and run scripts/localvm-setup.sh")
	}
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "genisoimage", "virt-customize"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("local provider needs %s; run scripts/localvm-setup.sh", bin)
		}
	}
	if _, err := os.Stat(localHelperPath); err != nil {
		return fmt.Errorf("local provider needs %s; run scripts/localvm-setup.sh", localHelperPath)
	}
	if err := exec.Command("sudo", "-n", "-l", localHelperPath).Run(); err != nil {
		return fmt.Errorf("sudoers rule for %s is missing; run scripts/localvm-setup.sh", localHelperPath)
	}
	return nil
}

func runLocalHelper(ctx context.Context, args ...string) error {
	_, err := runLocalHelperOutput(ctx, args...)
	return err
}

func runLocalHelperOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", localHelperPath}, args...)...)
	output, err := cmd.CombinedOutput()
	message := strings.TrimSpace(string(output))
	if err != nil {
		if message == "" {
			message = err.Error()
		}
		return message, fmt.Errorf("localvm network helper %v: %s", args, message)
	}
	return message, nil
}

func parseLocalHelperUplink(output string) string {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "uplink=") {
			return strings.TrimPrefix(field, "uplink=")
		}
	}
	return ""
}

type localVMRecord struct {
	Role    string `json:"role"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	IPv6    string `json:"ipv6"`
	Tap     string `json:"tap"`
	MAC     string `json:"mac"`
	Overlay string `json:"overlay"`
	Seed    string `json:"seed"`
	Console string `json:"console"`
	PIDFile string `json:"pid_file"`
}

type localResources struct {
	RunID     string          `json:"run_id"`
	Bridge    string          `json:"bridge"`
	Subnet    string          `json:"subnet"`
	Uplink    string          `json:"uplink,omitempty"`
	BaseImage string          `json:"base_image"`
	VMs       []localVMRecord `json:"vms"`
	Destroyed bool            `json:"destroyed"`
}

func writeLocalResources(path string, state localResources) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func readLocalResources(path string) (localResources, error) {
	var state localResources
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func planLocalVM(plan localPlan, role string, index int, runDir string) (localVMRecord, error) {
	network, _, err := localNetwork(plan.Subnet)
	if err != nil {
		return localVMRecord{}, err
	}
	ipv6Network, _, err := localIPv6Network(plan.Subnet)
	if err != nil {
		return localVMRecord{}, err
	}
	roleDir := filepath.Join(runDir, role)
	vm := localVMRecord{
		Role:    role,
		Name:    plan.RunID + "-" + role,
		IP:      localRoleIP(network, index).String(),
		IPv6:    localRoleIPv6(ipv6Network, index).String(),
		Tap:     localTapName(plan.RunID, index),
		MAC:     fmt.Sprintf("%s:%02x", localMACPrefix, index+1),
		Overlay: filepath.Join(roleDir, "disk.qcow2"),
		Seed:    filepath.Join(roleDir, "seed.iso"),
		Console: filepath.Join(roleDir, "console.log"),
		PIDFile: filepath.Join(roleDir, "qemu.pid"),
	}
	return vm, nil
}

// localTapName derives a run-unique tap name so parallel local runs cannot
// delete and steal each other's taps. Linux interface names are capped at 15
// characters.
func localTapName(runID string, index int) string {
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("ebt%x%d", sum[:3], index)
}

func provisionLocal(ctx context.Context, o localOptions, plan localPlan, artifactRoot, keyPath string) (map[string]hostInfo, func() error, error) {
	if err := checkLocalPrereqs(); err != nil {
		return nil, nil, err
	}
	network, gateway, err := localNetwork(plan.Subnet)
	if err != nil {
		return nil, nil, err
	}
	ones, _ := network.Mask.Size()
	runDir := filepath.Join(artifactRoot, "local-vms")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, nil, err
	}
	manifestPath := filepath.Join(artifactRoot, "local-resources.json")
	if _, err := os.Stat(manifestPath); err == nil {
		return nil, nil, errors.New("local resource manifest already exists; use a fresh artifact directory")
	}
	state := localResources{RunID: plan.RunID, Bridge: plan.Bridge, Subnet: network.String(), BaseImage: plan.BaseImage}
	if err := writeLocalResources(manifestPath, state); err != nil {
		return nil, nil, err
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return destroyLocalResources(cleanupCtx, manifestPath, o.keepDisks)
	}

	gatewayCIDR := fmt.Sprintf("%s/%d", gateway.String(), ones)
	upOutput, err := runLocalHelperOutput(ctx, "bridge-up", plan.Bridge, gatewayCIDR, network.String())
	if err != nil {
		return nil, cleanup, err
	}
	state.Uplink = parseLocalHelperUplink(upOutput)
	if err := writeLocalResources(manifestPath, state); err != nil {
		return nil, cleanup, err
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, cleanup, err
	}
	userName, err := localTapOwner()
	if err != nil {
		return nil, cleanup, err
	}
	cacheDir, err := localCacheDir(o.cacheDir)
	if err != nil {
		return nil, cleanup, err
	}
	preparedPath, err := newLocalImageManager(cacheDir).ensure(ctx, plan.BaseImage, localImageRecipe(plan.BaseSHA, plan.AptMirror))
	if err != nil {
		return nil, cleanup, err
	}
	roles := localRoles(plan.Agents)
	hosts := make(map[string]hostInfo, len(roles))
	for index, role := range roles {
		if err := ctx.Err(); err != nil {
			return nil, cleanup, err
		}
		vm, err := planLocalVM(plan, role, index, runDir)
		if err != nil {
			return nil, cleanup, err
		}
		state.VMs = append(state.VMs, vm)
		if err := writeLocalResources(manifestPath, state); err != nil {
			return nil, cleanup, err
		}
		if err := prepareLocalVM(ctx, plan, vm, ones, strings.TrimSpace(string(pub)), preparedPath); err != nil {
			return nil, cleanup, err
		}
		if err := runLocalHelper(ctx, "tap-add", plan.Bridge, vm.Tap, userName); err != nil {
			return nil, cleanup, err
		}
		if err := startLocalVM(ctx, plan, vm); err != nil {
			return nil, cleanup, err
		}
		roleName := "agent"
		if role == "controlplane" {
			roleName = "controlplane"
		}
		hosts[role] = hostInfo{Role: roleName, Name: vm.Name, PublicIPv4: vm.IP, PublicIPv6: vm.IPv6}
		infof("local: started %s at %s (%s)", role, vm.IP, vm.Name)
	}
	return hosts, cleanup, nil
}

func prepareLocalVM(ctx context.Context, plan localPlan, vm localVMRecord, prefix int, publicKey, preparedImage string) error {
	roleDir := filepath.Dir(vm.Overlay)
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", preparedImage, vm.Overlay).CombinedOutput(); err != nil {
		return fmt.Errorf("create overlay for %s: %w: %s", vm.Role, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "resize", vm.Overlay, strconv.Itoa(plan.DiskGiB)+"G").CombinedOutput(); err != nil {
		return fmt.Errorf("resize overlay for %s: %w: %s", vm.Role, err, strings.TrimSpace(string(out)))
	}
	if err := writeLocalSeed(ctx, plan, vm, prefix, publicKey); err != nil {
		return err
	}
	return nil
}

func writeLocalSeed(ctx context.Context, plan localPlan, vm localVMRecord, prefix int, publicKey string) error {
	// The prepared image already contains every package and enables containerd,
	// so the per-VM seed only carries identity and networking.
	userData := fmt.Sprintf(`#cloud-config
hostname: %s
disable_root: false
ssh_pwauth: false
users:
  - default
  - name: root
    ssh_authorized_keys:
      - %s
`, vm.Name, publicKey)

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vm.Name, vm.Name)
	gateway := strings.SplitN(plan.Gateway, "/", 2)[0]
	networkConfig := localNetworkConfig(vm, gateway, prefix)
	roleDir := filepath.Dir(vm.Seed)
	userDataPath := filepath.Join(roleDir, "user-data")
	metaDataPath := filepath.Join(roleDir, "meta-data")
	networkPath := filepath.Join(roleDir, "network-config")
	for path, content := range map[string]string{userDataPath: userData, metaDataPath: metaData, networkPath: networkConfig} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return runCommand(ctx, roleDir, os.Environ(), "genisoimage",
		"-quiet", "-input-charset", "utf-8", "-output", vm.Seed,
		"-volid", "cidata", "-joliet", "-rock",
		userDataPath, metaDataPath, networkPath)
}

func localNetworkConfig(vm localVMRecord, gateway string, prefix int) string {
	return fmt.Sprintf(`version: 2
ethernets:
  mesh0:
    match:
      macaddress: "%s"
    mtu: 1400
    dhcp4: false
    dhcp6: false
    addresses:
      - %s/%d
      - %s/64
    routes:
      - to: default
        via: %s
    nameservers:
      addresses: [%s]
`, vm.MAC, vm.IP, prefix, vm.IPv6, gateway, strings.Join(localNameServers(), ", "))
}

func startLocalVM(ctx context.Context, plan localPlan, vm localVMRecord) error {
	vcpus, memory := plan.AgentVCPUs, plan.AgentMemMiB
	if vm.Role == "controlplane" {
		vcpus, memory = plan.ControlPlaneVCPUs, plan.ControlPlaneMemMiB
	}
	args := []string{
		"-name", vm.Name,
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(vcpus),
		"-m", strconv.Itoa(memory),
		"-drive", "file=" + qemuOptionPath(vm.Overlay) + ",if=virtio,format=qcow2,cache=writeback",
		"-drive", "file=" + qemuOptionPath(vm.Seed) + ",if=virtio,format=raw,readonly=on",
		"-netdev", "tap,id=net0,ifname=" + vm.Tap + ",script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac=" + vm.MAC,
		"-device", "virtio-rng-pci",
		"-serial", "file:" + qemuOptionPath(vm.Console),
		"-display", "none",
		"-no-reboot",
		"-daemonize",
		"-pidfile", vm.PIDFile,
	}
	if err := runCommand(ctx, filepath.Dir(vm.Overlay), os.Environ(), "qemu-system-x86_64", args...); err != nil {
		return fmt.Errorf("start qemu for %s: %w", vm.Role, err)
	}
	return nil
}

func qemuOptionPath(path string) string {
	return strings.ReplaceAll(path, ",", ",,")
}

func (o localOptions) aptMirrorValue() string {
	if mirror := strings.TrimSpace(o.aptMirror); mirror != "" {
		return strings.TrimRight(mirror, "/")
	}
	return detectLocalAptMirror()
}

func detectLocalAptMirror() string {
	for _, path := range []string{"/etc/apt/sources.list.d/ubuntu.sources", "/etc/apt/sources.list"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			var uri string
			switch {
			case strings.HasPrefix(line, "URIs:"):
				if fields := strings.Fields(strings.TrimPrefix(line, "URIs:")); len(fields) > 0 {
					uri = fields[0]
				}
			case strings.HasPrefix(line, "deb "):
				if fields := strings.Fields(line); len(fields) >= 2 {
					uri = fields[1]
				}
			}
			if strings.Contains(uri, "ubuntu.com") && !strings.Contains(uri, "security.ubuntu.com") {
				mirror := strings.TrimRight(uri, "/")
				if err := validateAptMirror(mirror); err != nil {
					continue
				}
				return mirror
			}
		}
	}
	return ""
}

func localNameServers() []string {
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		var servers []string
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "nameserver" {
				continue
			}
			ip := net.ParseIP(fields[1])
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			servers = append(servers, fields[1])
			if len(servers) == 2 {
				break
			}
		}
		if len(servers) > 0 {
			return servers
		}
	}
	return []string{"1.1.1.1", "8.8.8.8"}
}

func destroyLocal(ctx context.Context, manifestPath string, keepDisks bool) error {
	if strings.TrimSpace(manifestPath) == "" {
		return errors.New("local-manifest is required for local-action destroy")
	}
	return destroyLocalResources(ctx, manifestPath, keepDisks)
}

func destroyLocalResources(ctx context.Context, manifestPath string, keepDisks bool) error {
	state, err := readLocalResources(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			infof("local: no resource manifest at %s; nothing to destroy", manifestPath)
			return nil
		}
		return err
	}
	if state.Destroyed {
		if keepDisks {
			return nil
		}
		var failures []error
		for _, vm := range state.VMs {
			for _, path := range localVMAuxiliaryPaths(vm, true) {
				if strings.TrimSpace(path) == "" {
					continue
				}
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					failures = append(failures, fmt.Errorf("remove %s: %w", path, err))
				}
			}
		}
		return errors.Join(failures...)
	}
	var failures []error
	for _, vm := range state.VMs {
		if err := stopLocalVM(ctx, vm); err != nil {
			failures = append(failures, fmt.Errorf("stop %s: %w", vm.Role, err))
		}
		if err := runLocalHelper(ctx, "tap-del", vm.Tap); err != nil {
			failures = append(failures, fmt.Errorf("delete tap %s: %w", vm.Tap, err))
		}
		if !keepDisks {
			for _, path := range localVMAuxiliaryPaths(vm, true) {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					failures = append(failures, fmt.Errorf("remove %s: %w", path, err))
				}
			}
		} else {
			for _, path := range localVMAuxiliaryPaths(vm, false) {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					failures = append(failures, fmt.Errorf("remove %s: %w", path, err))
				}
			}
		}
	}
	downArgs := []string{"bridge-down", state.Bridge, state.Subnet}
	if state.Uplink != "" {
		downArgs = append(downArgs, state.Uplink)
	}
	if err := runLocalHelper(ctx, downArgs...); err != nil {
		failures = append(failures, fmt.Errorf("bridge down: %w", err))
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	state.Destroyed = true
	return writeLocalResources(manifestPath, state)
}

func localVMAuxiliaryPaths(vm localVMRecord, includeDisks bool) []string {
	paths := make([]string, 0, 7)
	for _, path := range []string{vm.Console, vm.PIDFile} {
		if path != "" {
			paths = append(paths, path)
		}
	}
	if vm.Seed != "" {
		roleDir := filepath.Dir(vm.Seed)
		paths = append(paths,
			filepath.Join(roleDir, "user-data"),
			filepath.Join(roleDir, "meta-data"),
			filepath.Join(roleDir, "network-config"),
		)
	}
	if includeDisks {
		for _, path := range []string{vm.Overlay, vm.Seed} {
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func stopLocalVM(ctx context.Context, vm localVMRecord) error {
	data, err := os.ReadFile(vm.PIDFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return err
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "qemu-system-") || !containsArgPair(args, "-pidfile", vm.PIDFile) {
		return fmt.Errorf("refusing to signal pid %d: pidfile does not identify the expected qemu process", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
	err = testutil.Poll(ctx, testutil.PollConfig{Timeout: 20 * time.Second, Interval: 500 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		return process.Signal(syscall.Signal(0)) != nil, nil
	})
	if err != nil {
		if killErr := process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return errors.Join(err, killErr)
		}
		if waitErr := testutil.Poll(ctx, testutil.PollConfig{Timeout: 5 * time.Second, Interval: 250 * time.Millisecond}, func(context.Context) (bool, error) {
			return process.Signal(syscall.Signal(0)) != nil, nil
		}); waitErr != nil {
			return fmt.Errorf("qemu pid %d survived SIGKILL: %w", pid, waitErr)
		}
	}
	return nil
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
