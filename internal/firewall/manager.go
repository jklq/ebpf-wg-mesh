//go:build linux

package firewall

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	containerd "github.com/containerd/containerd"
	eventsapi "github.com/containerd/containerd/api/events"
	cderrdefs "github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/meshlabels"
)

const (
	roleWireGuard uint8 = 1
	roleContainer uint8 = 2
)

type containerRuntime struct {
	containerID     string
	ifindex         uint32
	ipv4            netip.Addr
	ipv6            netip.Addr
	networkIdentity uint32
	innerMap        *ebpf.Map
	ingressLink     link.Link
	egressLink      link.Link
}

type Manager struct {
	cfg             config.MeshRuntimeConfig
	labelKeys       meshlabels.Keys
	objs            firewallObjects
	wgIfindex       uint32
	wgIngressLink   link.Link
	wgEgressLink    link.Link
	containerd      *containerd.Client
	cancel          context.CancelFunc
	done            chan struct{}
	mu              sync.Mutex
	containers      map[string]*containerRuntime
	staticByID      map[string]config.ContainerAssignment
	seedByIP        map[[16]byte]firewallIdentityValue
	configuredByKey map[firewallIdentityKey]firewallIdentityValue
	localHostIP     [16]byte
}

func Start(ctx context.Context, cfg config.MeshRuntimeConfig) (_ *Manager, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}

	localHost, err := netip.ParseAddr(cfg.Host.IPv6)
	if err != nil || !localHost.Is6() {
		return nil, fmt.Errorf("parse host.ipv6 %q: %w", cfg.Host.IPv6, err)
	}
	localHostIP := addrAs16(localHost)

	wgIface, err := net.InterfaceByName(cfg.WireGuard.InterfaceName)
	if err != nil {
		return nil, fmt.Errorf("lookup interface %s: %w", cfg.WireGuard.InterfaceName, err)
	}

	spec, err := loadFirewall()
	if err != nil {
		return nil, fmt.Errorf("load bpf spec: %w", err)
	}
	if ms, ok := spec.Maps["conntrack_matrix"]; ok {
		if cfg.Firewall.MaxContainers > 0 {
			ms.MaxEntries = uint32(cfg.Firewall.MaxContainers)
		}
		if ms.InnerMap != nil && cfg.Firewall.ConntrackInnerEntries > 0 {
			ms.InnerMap.MaxEntries = uint32(cfg.Firewall.ConntrackInnerEntries)
		}
	}
	if ms, ok := spec.Maps["cluster_identity_trie"]; ok && cfg.Firewall.ClusterIdentityEntries > 0 {
		ms.MaxEntries = uint32(cfg.Firewall.ClusterIdentityEntries)
	}
	if ms, ok := spec.Maps["container_policy_map"]; ok && cfg.Firewall.MaxContainers > 0 {
		ms.MaxEntries = uint32(cfg.Firewall.MaxContainers)
	}
	if ms, ok := spec.Maps["interface_role_map"]; ok && cfg.Firewall.MaxContainers > 0 {
		ms.MaxEntries = uint32(cfg.Firewall.MaxContainers + 16)
	}

	objs := firewallObjects{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign eBPF objects: %w", err)
	}
	cleanupObjs := true
	defer func() {
		if cleanupObjs {
			objs.Close()
		}
	}()

	zero := uint32(0)
	if err := objs.LocalNodeMap.Put(zero, localHostIP); err != nil {
		return nil, fmt.Errorf("set local host map: %w", err)
	}
	configuredSeeds, err := configuredIdentities(cfg)
	if err != nil {
		return nil, err
	}
	seedByIP := make(map[[16]byte]firewallIdentityValue, len(cfg.Containerd.IdentitySeeds))
	configuredByKey := make(map[firewallIdentityKey]firewallIdentityValue, len(configuredSeeds))
	for _, seed := range configuredSeeds {
		key := identityKeyForPrefix(seed.prefix)
		value := firewallIdentityValue{
			NetworkIdentity: seed.networkIdentity,
			VethIfindex:     0,
		}
		if seed.hostIPv6.IsValid() {
			value.HostIp = addrAs16(seed.hostIPv6)
		}
		if err := objs.ClusterIdentityTrie.Put(key, value); err != nil {
			return nil, fmt.Errorf("seed identity trie for prefix %s: %w", seed.prefix.String(), err)
		}
		configuredByKey[key] = value
		if key.Prefixlen == 128 {
			seedByIP[key.IpAddress] = value
		}
	}
	wgIfindex := uint32(wgIface.Index)
	if err := objs.InterfaceRoleMap.Put(wgIfindex, roleWireGuard); err != nil {
		return nil, fmt.Errorf("set wg interface role: %w", err)
	}

	wgIngress, err := link.AttachTCX(link.TCXOptions{
		Interface: wgIface.Index,
		Attach:    ebpf.AttachTCXIngress,
		Program:   objs.TcxIngress,
	})
	if err != nil {
		return nil, fmt.Errorf("attach wg ingress tcx: %w", err)
	}
	cleanupWgIngress := true
	defer func() {
		if cleanupWgIngress {
			wgIngress.Close()
		}
	}()

	wgEgress, err := link.AttachTCX(link.TCXOptions{
		Interface: wgIface.Index,
		Attach:    ebpf.AttachTCXEgress,
		Program:   objs.TcxEgress,
	})
	if err != nil {
		return nil, fmt.Errorf("attach wg egress tcx: %w", err)
	}
	cleanupWgEgress := true
	defer func() {
		if cleanupWgEgress {
			wgEgress.Close()
		}
	}()

	staticByID := make(map[string]config.ContainerAssignment, len(cfg.Containerd.StaticAssignments))
	for _, assignment := range cfg.Containerd.StaticAssignments {
		if assignment.ContainerID != "" {
			staticByID[assignment.ContainerID] = assignment
		}
	}

	client, err := containerd.New(
		cfg.Containerd.Socket,
		containerd.WithDefaultNamespace(cfg.Containerd.Namespace),
	)
	if err != nil {
		return nil, fmt.Errorf("connect containerd %s: %w", cfg.Containerd.Socket, err)
	}
	cleanupClient := true
	defer func() {
		if cleanupClient {
			client.Close()
		}
	}()

	eventsCtx, cancel := context.WithCancel(ctx)
	m := &Manager{
		cfg:             cfg,
		labelKeys:       cfg.Containerd.LabelKeys(),
		objs:            objs,
		wgIfindex:       wgIfindex,
		wgIngressLink:   wgIngress,
		wgEgressLink:    wgEgress,
		containerd:      client,
		cancel:          cancel,
		done:            make(chan struct{}),
		containers:      make(map[string]*containerRuntime),
		staticByID:      staticByID,
		seedByIP:        seedByIP,
		configuredByKey: configuredByKey,
		localHostIP:     localHostIP,
	}

	cleanupObjs = false
	cleanupWgIngress = false
	cleanupWgEgress = false
	cleanupClient = false

	go m.eventLoop(eventsCtx)
	return m, nil
}

func (m *Manager) UpdateIdentityCatalog(cfg config.MeshRuntimeConfig) error {
	if m == nil {
		return errors.New("firewall manager is not running")
	}
	configured, err := configuredIdentities(cfg)
	if err != nil {
		return err
	}
	next := make(map[firewallIdentityKey]firewallIdentityValue, len(configured))
	nextSeeds := make(map[[16]byte]firewallIdentityValue, len(cfg.Containerd.IdentitySeeds))
	for _, seed := range configured {
		key := identityKeyForPrefix(seed.prefix)
		value := firewallIdentityValue{NetworkIdentity: seed.networkIdentity, VethIfindex: 0}
		if seed.hostIPv6.IsValid() {
			value.HostIp = addrAs16(seed.hostIPv6)
		}
		next[key] = value
		if key.Prefixlen == 128 {
			nextSeeds[key.IpAddress] = value
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for key := range m.configuredByKey {
		if _, retained := next[key]; retained {
			continue
		}
		if err := m.objs.ClusterIdentityTrie.Delete(key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("delete identity trie entry for %s: %w", netip.AddrFrom16(key.IpAddress), err)
		}
	}
	for key, value := range next {
		value = effectiveIdentityValue(key, value, m.containers, m.localHostIP)
		next[key] = value
		if current, exists := m.configuredByKey[key]; exists && current == value {
			continue
		}
		if err := m.objs.ClusterIdentityTrie.Put(key, value); err != nil {
			return fmt.Errorf("update identity trie entry for %s: %w", netip.AddrFrom16(key.IpAddress), err)
		}
	}
	m.configuredByKey = next
	m.seedByIP = nextSeeds
	m.cfg = cfg
	return nil
}

func (m *Manager) eventLoop(ctx context.Context) {
	defer close(m.done)

	for {
		if ctx.Err() != nil {
			return
		}

		eventsCh, errCh := m.containerd.EventService().Subscribe(ctx)
		m.reconcileExistingTasks(ctx)

		resubscribe := false
		for !resubscribe {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-errCh:
				if !ok {
					resubscribe = true
					continue
				}
				if err == nil {
					continue
				}
				slog.Warn("containerd event subscription dropped", "error", err)
				resubscribe = true
			case envelope, ok := <-eventsCh:
				if !ok {
					resubscribe = true
					continue
				}
				if envelope == nil || envelope.Event == nil {
					continue
				}
				evt, err := typeurl.UnmarshalAny(envelope.Event)
				if err != nil {
					slog.Warn("unmarshal containerd event", "error", err, "topic", envelope.Topic)
					continue
				}
				switch e := evt.(type) {
				case *eventsapi.TaskStart:
					if err := m.handleTaskStart(ctx, e); err != nil {
						slog.Warn("task start handling failed", "container", e.ContainerID, "pid", e.Pid, "error", err)
					}
				case *eventsapi.TaskExit:
					if err := m.handleTaskExit(e.ContainerID, e.ID); err != nil {
						slog.Warn("task exit handling failed", "container", e.ContainerID, "id", e.ID, "error", err)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) reconcileExistingTasks(ctx context.Context) {
	containers, err := m.containerd.Containers(ctx)
	if err != nil {
		slog.Warn("list containers for initial firewall reconciliation failed", "error", err)
		return
	}

	for _, ctr := range containers {
		if ctr == nil {
			continue
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			if cderrdefs.IsNotFound(err) {
				continue
			}
			slog.Debug("skip container without active task", "container", ctr.ID(), "error", err)
			continue
		}
		pid := task.Pid()
		if pid == 0 {
			continue
		}
		evt := &eventsapi.TaskStart{
			ContainerID: ctr.ID(),
			Pid:         pid,
		}
		if err := m.handleTaskStart(ctx, evt); err != nil {
			slog.Warn("initial firewall reconciliation failed",
				"container", ctr.ID(),
				"pid", pid,
				"error", err,
			)
		}
	}
}

func (m *Manager) handleTaskStart(ctx context.Context, evt *eventsapi.TaskStart) error {
	if evt == nil {
		return errors.New("nil task start event")
	}
	if evt.ContainerID == "" {
		return errors.New("empty container id")
	}
	if evt.Pid == 0 {
		return fmt.Errorf("task start for %q has zero pid", evt.ContainerID)
	}

	identity, err := m.resolveContainerIdentity(ctx, evt.ContainerID)
	if err != nil {
		return err
	}

	ifindex, err := resolveHostVethIfindexWithRetry(ctx, evt.Pid, 20, 150*time.Millisecond)
	if err != nil {
		return fmt.Errorf("resolve host veth for pid %d: %w", evt.Pid, err)
	}
	if ifindex <= 0 {
		return fmt.Errorf("invalid ifindex %d for pid %d", ifindex, evt.Pid)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	ipv4Seed, ipv4Known := m.seedByIP[addrAs16(identity.IPv4)]
	ipv6Seed, ipv6Known := m.seedByIP[addrAs16(identity.IPv6)]
	if !ipv4Known || !ipv6Known ||
		ipv4Seed.NetworkIdentity != identity.NetworkIdentity ||
		ipv6Seed.NetworkIdentity != identity.NetworkIdentity ||
		ipv4Seed.NetworkIdentity != ipv6Seed.NetworkIdentity {
		return fmt.Errorf("container %s dual-stack identity is absent from the assigned catalog", evt.ContainerID)
	}

	if existing := m.containers[evt.ContainerID]; existing != nil {
		_ = m.removeContainerLocked(existing)
	}

	innerSpec := &ebpf.MapSpec{
		Type:       ebpf.LRUHash,
		KeySize:    uint32(binary.Size(firewallConnectionKey{})),
		ValueSize:  8,
		MaxEntries: uint32(m.cfg.Firewall.ConntrackInnerEntries),
	}
	innerMap, err := ebpf.NewMap(innerSpec)
	if err != nil {
		return fmt.Errorf("create conntrack inner map for %s: %w", evt.ContainerID, err)
	}
	ifKey := uint32(ifindex)
	if err := m.objs.ConntrackMatrix.Put(ifKey, innerMap); err != nil {
		innerMap.Close()
		return fmt.Errorf("insert inner map into conntrack_matrix: %w", err)
	}

	policy := firewallContainerPolicy{
		NetworkIdentity: identity.NetworkIdentity,
		Ipv4:            identity.IPv4.As4(),
		Ipv6:            addrAs16(identity.IPv6),
	}
	if err := m.objs.ContainerPolicyMap.Put(ifKey, policy); err != nil {
		_ = m.objs.ConntrackMatrix.Delete(ifKey)
		innerMap.Close()
		return fmt.Errorf("write container policy: %w", err)
	}
	if err := m.objs.InterfaceRoleMap.Put(ifKey, roleContainer); err != nil {
		_ = m.objs.ContainerPolicyMap.Delete(ifKey)
		_ = m.objs.ConntrackMatrix.Delete(ifKey)
		innerMap.Close()
		return fmt.Errorf("write interface role: %w", err)
	}

	identityValue := firewallIdentityValue{
		NetworkIdentity: identity.NetworkIdentity,
		HostIp:          m.localHostIP,
		VethIfindex:     ifKey,
	}
	runtime := &containerRuntime{
		containerID:     evt.ContainerID,
		ifindex:         ifKey,
		networkIdentity: identity.NetworkIdentity,
		innerMap:        innerMap,
	}
	for _, addr := range []netip.Addr{identity.IPv4, identity.IPv6} {
		identityKey := identityKeyForPrefix(netip.PrefixFrom(addr, addr.BitLen()))
		if err := m.objs.ClusterIdentityTrie.Put(identityKey, identityValue); err != nil {
			_ = m.removeContainerLocked(runtime)
			return fmt.Errorf("write identity trie entry: %w", err)
		}
		if addr.Is4() {
			runtime.ipv4 = addr
		} else {
			runtime.ipv6 = addr
		}
	}

	ingress, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Attach:    ebpf.AttachTCXIngress,
		Program:   m.objs.TcxIngress,
	})
	if err != nil {
		_ = m.removeContainerLocked(runtime)
		return fmt.Errorf("attach veth ingress tcx: %w", err)
	}
	runtime.ingressLink = ingress

	egress, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Attach:    ebpf.AttachTCXEgress,
		Program:   m.objs.TcxEgress,
	})
	if err != nil {
		_ = m.removeContainerLocked(runtime)
		return fmt.Errorf("attach veth egress tcx: %w", err)
	}
	runtime.egressLink = egress
	m.containers[evt.ContainerID] = runtime
	slog.Info("container firewall attached",
		"container", evt.ContainerID,
		"pid", evt.Pid,
		"ifindex", ifindex,
		"networkIdentity", identity.NetworkIdentity,
		"ipv4", identity.IPv4.String(),
		"ipv6", identity.IPv6.String(),
	)

	return nil
}

func (m *Manager) handleTaskExit(containerID, taskID string) error {
	if containerID == "" {
		return errors.New("empty container id")
	}
	// containerd emits TaskExit for `nerdctl exec` processes with non-empty IDs.
	// Those events must not tear down networking for the still-running container task.
	if taskID != "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	runtime := m.containers[containerID]
	if runtime == nil {
		return nil
	}
	if err := m.removeContainerLocked(runtime); err != nil {
		return err
	}
	delete(m.containers, containerID)
	slog.Info("container firewall detached", "container", containerID)
	return nil
}

func (m *Manager) removeContainerLocked(runtime *containerRuntime) error {
	if runtime == nil {
		return nil
	}
	var errs []error
	if runtime.ingressLink != nil {
		if err := runtime.ingressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ingress link: %w", err))
		}
	}
	if runtime.egressLink != nil {
		if err := runtime.egressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close egress link: %w", err))
		}
	}
	if runtime.ifindex != 0 {
		ifkey := runtime.ifindex
		if err := m.objs.InterfaceRoleMap.Delete(ifkey); err != nil {
			errs = append(errs, fmt.Errorf("delete interface role: %w", err))
		}
		if err := m.objs.ContainerPolicyMap.Delete(ifkey); err != nil {
			errs = append(errs, fmt.Errorf("delete container policy: %w", err))
		}
		if err := m.objs.ConntrackMatrix.Delete(ifkey); err != nil {
			errs = append(errs, fmt.Errorf("delete conntrack matrix entry: %w", err))
		}
	}
	for _, addr := range []netip.Addr{runtime.ipv4, runtime.ipv6} {
		if !addr.IsValid() {
			continue
		}
		ip16 := addrAs16(addr)
		identityKey := identityKeyForPrefix(netip.PrefixFrom(addr, addr.BitLen()))
		if seedValue, ok := m.seedByIP[ip16]; ok {
			if err := m.objs.ClusterIdentityTrie.Put(identityKey, seedValue); err != nil {
				errs = append(errs, fmt.Errorf("restore seeded identity trie entry: %w", err))
			}
		} else {
			if err := m.objs.ClusterIdentityTrie.Delete(identityKey); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
				errs = append(errs, fmt.Errorf("delete identity trie entry: %w", err))
			}
		}
	}
	if runtime.innerMap != nil {
		if err := runtime.innerMap.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close inner map: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) resolveContainerIdentity(ctx context.Context, containerID string) (meshlabels.Identity, error) {
	if assignment, ok := m.staticByID[containerID]; ok {
		ipv4, err := netip.ParseAddr(assignment.IPv4)
		if err != nil || !ipv4.Is4() {
			return meshlabels.Identity{}, fmt.Errorf("static assignment invalid ipv4 for %s: %w", containerID, err)
		}
		ipv6, err := netip.ParseAddr(assignment.IPv6)
		if err != nil || !ipv6.Is6() {
			return meshlabels.Identity{}, fmt.Errorf("static assignment invalid ipv6 for %s: %w", containerID, err)
		}
		return meshlabels.Identity{
			NetworkIdentity: assignment.NetworkIdentity,
			IPv4:            ipv4,
			IPv6:            ipv6,
		}, nil
	}

	container, err := m.containerd.LoadContainer(ctx, containerID)
	if err != nil {
		return meshlabels.Identity{}, fmt.Errorf("load container %s: %w", containerID, err)
	}
	info, err := container.Info(ctx)
	if err != nil {
		return meshlabels.Identity{}, fmt.Errorf("container info %s: %w", containerID, err)
	}

	identity, err := m.labelKeys.Decode(info.Labels)
	if err != nil {
		return meshlabels.Identity{}, fmt.Errorf("container %s: %w", containerID, err)
	}
	return identity, nil
}

func resolveHostVethIfindex(pid uint32) (int, error) {
	nsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	targetNS, err := netns.GetFromPath(nsPath)
	if err != nil {
		return 0, fmt.Errorf("open netns %s: %w", nsPath, err)
	}
	defer targetNS.Close()

	hostNS, err := netns.Get()
	if err != nil {
		return 0, fmt.Errorf("get host netns: %w", err)
	}
	defer hostNS.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := netns.Set(targetNS); err != nil {
		return 0, fmt.Errorf("enter target netns: %w", err)
	}
	defer func() {
		_ = netns.Set(hostNS)
	}()

	insideLink, err := netlink.LinkByName("eth0")
	if err != nil {
		return 0, fmt.Errorf("lookup eth0 in netns: %w", err)
	}
	peerIfindex := insideLink.Attrs().ParentIndex
	if peerIfindex <= 0 {
		return 0, fmt.Errorf("eth0 parent index missing for pid %d", pid)
	}

	if err := netns.Set(hostNS); err != nil {
		return 0, fmt.Errorf("restore host netns: %w", err)
	}

	hostLink, err := netlink.LinkByIndex(peerIfindex)
	if err != nil {
		return 0, fmt.Errorf("lookup host peer link %d: %w", peerIfindex, err)
	}
	return hostLink.Attrs().Index, nil
}

func resolveHostVethIfindexWithRetry(ctx context.Context, pid uint32, attempts int, delay time.Duration) (int, error) {
	if attempts <= 1 {
		return resolveHostVethIfindex(pid)
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		ifindex, err := resolveHostVethIfindex(pid)
		if err == nil {
			return ifindex, nil
		}
		lastErr = err

		if ctx != nil {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(delay):
			}
		} else {
			time.Sleep(delay)
		}
	}
	return 0, lastErr
}

func effectiveIdentityValue(key firewallIdentityKey, value firewallIdentityValue, containers map[string]*containerRuntime, localHostIP [16]byte) firewallIdentityValue {
	if key.Prefixlen == 128 {
		if runtime := localRuntimeByIP(containers, key.IpAddress); runtime != nil {
			value.HostIp = localHostIP
			value.VethIfindex = runtime.ifindex
		}
	}
	return value
}

func localRuntimeByIP(containers map[string]*containerRuntime, ip [16]byte) *containerRuntime {
	for _, runtime := range containers {
		if runtime != nil && ((runtime.ipv4.IsValid() && addrAs16(runtime.ipv4) == ip) ||
			(runtime.ipv6.IsValid() && addrAs16(runtime.ipv6) == ip)) {
			return runtime
		}
	}
	return nil
}

func addrAs16(addr netip.Addr) [16]byte {
	return addr.As16()
}

func identityKeyForPrefix(prefix netip.Prefix) firewallIdentityKey {
	prefix = prefix.Masked()
	bits := prefix.Bits()
	if prefix.Addr().Is4() {
		bits += 96
	}
	return firewallIdentityKey{
		Prefixlen: uint32(bits),
		IpAddress: addrAs16(prefix.Addr()),
	}
}

func (m *Manager) AttachedCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.containers)
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		<-m.done
	}

	m.mu.Lock()
	var errs []error
	for id, runtime := range m.containers {
		if err := m.removeContainerLocked(runtime); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", id, err))
		}
		delete(m.containers, id)
	}
	m.mu.Unlock()

	if m.wgIngressLink != nil {
		if err := m.wgIngressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close wg ingress link: %w", err))
		}
	}
	if m.wgEgressLink != nil {
		if err := m.wgEgressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close wg egress link: %w", err))
		}
	}
	if m.objs.InterfaceRoleMap != nil && m.wgIfindex != 0 {
		if err := m.objs.InterfaceRoleMap.Delete(m.wgIfindex); err != nil {
			errs = append(errs, fmt.Errorf("delete wg role entry: %w", err))
		}
	}
	if err := m.objs.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close eBPF objects: %w", err))
	}
	if m.containerd != nil {
		if err := m.containerd.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close containerd client: %w", err))
		}
	}
	return errors.Join(errs...)
}
