package firewall

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"networking-rig/internal/config"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
)

type Manager struct {
	objs        firewallObjects
	ingressLink link.Link
	egressLink  link.Link
	events      *ringbuf.Reader
	syncRuntime *SyncRuntime
}

func Attach(ifaceName string, fwCfg config.FirewallConfig, trustCIDRs []netip.Prefix) (_ *Manager, retErr error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("lookup interface %s: %w", ifaceName, err)
	}

	spec, err := loadFirewall()
	if err != nil {
		return nil, fmt.Errorf("load bpf spec: %w", err)
	}

	if ms, ok := spec.Maps["conntrack_map"]; ok && fwCfg.ConntrackEntries > 0 {
		ms.MaxEntries = uint32(fwCfg.ConntrackEntries)
	}
	if ms, ok := spec.Maps["mesh_trust_map"]; ok && fwCfg.TrustEntries > 0 {
		ms.MaxEntries = uint32(fwCfg.TrustEntries)
	}

	objs := firewallObjects{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("load and assign eBPF objects: %w", ve)
		}
		return nil, fmt.Errorf("load and assign eBPF objects: %w", err)
	}
	cleanupObjs := true
	defer func() {
		if cleanupObjs {
			objs.Close()
		}
	}()

	ingress, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Attach:    ebpf.AttachTCXIngress,
		Program:   objs.TcxIngress,
	})
	if err != nil {
		return nil, fmt.Errorf("attach tcx ingress on %s: %w", ifaceName, err)
	}
	cleanupIngress := true
	defer func() {
		if cleanupIngress {
			ingress.Close()
		}
	}()

	egress, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Attach:    ebpf.AttachTCXEgress,
		Program:   objs.TcxEgress,
	})
	if err != nil {
		return nil, fmt.Errorf("attach tcx egress on %s: %w", ifaceName, err)
	}
	cleanupEgress := true
	defer func() {
		if cleanupEgress {
			egress.Close()
		}
	}()

	for _, prefix := range trustCIDRs {
		if !prefix.Addr().Is4() {
			continue
		}
		addr := prefix.Addr().As4()
		key := firewallLpmKey{
			Prefixlen: uint32(prefix.Bits()),
			Addr:      nativeU32(addr[:]),
		}
		val := uint8(1)
		if err := objs.MeshTrustMap.Put(key, val); err != nil {
			return nil, fmt.Errorf("insert trust cidr %s: %w", prefix, err)
		}
	}

	events, err := ringbuf.NewReader(objs.ConnEvents)
	if err != nil {
		return nil, fmt.Errorf("open ringbuf reader: %w", err)
	}

	cleanupObjs = false
	cleanupIngress = false
	cleanupEgress = false

	return &Manager{
		objs:        objs,
		ingressLink: ingress,
		egressLink:  egress,
		events:      events,
	}, nil
}

func (m *Manager) StartSync(nodeName, listen, authKey string, replayWindow time.Duration, peers []string) error {
	if m == nil {
		return errors.New("nil firewall manager")
	}
	if m.syncRuntime != nil {
		return nil
	}
	runtime, err := startSync(nodeName, listen, authKey, replayWindow, peers, m.events, m.objs.ConntrackMap)
	if err != nil {
		return err
	}
	m.syncRuntime = runtime
	return nil
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var errs []error
	if m.syncRuntime != nil {
		if err := m.syncRuntime.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if m.events != nil {
		if err := m.events.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ringbuf reader: %w", err))
		}
	}
	if m.ingressLink != nil {
		if err := m.ingressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ingress link: %w", err))
		}
	}
	if m.egressLink != nil {
		if err := m.egressLink.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close egress link: %w", err))
		}
	}
	if err := m.objs.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close eBPF objects: %w", err))
	}
	return errors.Join(errs...)
}

func nativeU32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	// Go stores integer fields in native endianness; using native parsing preserves raw bytes.
	return binary.NativeEndian.Uint32(b[:4])
}
