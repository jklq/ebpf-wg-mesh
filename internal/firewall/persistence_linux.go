//go:build linux

package firewall

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/meshstate"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

func pinPath(interfaceName string) (string, error) { return meshstate.PinPath(interfaceName) }

func openPinnedObjects(spec *ebpf.CollectionSpec, dir string, objs *firewallObjects) error {
	maps := filepath.Join(dir, "maps")
	if err := os.MkdirAll(maps, 0700); err != nil {
		return err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(maps, &stat); err != nil {
		return err
	}
	if stat.Type != unix.BPF_FS_MAGIC {
		return fmt.Errorf("%s must be on bpffs", maps)
	}
	for _, ms := range spec.Maps {
		ms.Pinning = ebpf.PinByName
	}
	// PinByName reopens existing maps and validates their layout. Incompatible
	// state is an error, never a reason to discard a running data plane.
	return spec.LoadAndAssign(objs, &ebpf.CollectionOptions{Maps: ebpf.MapOptions{PinPath: maps}})
}

func openPinnedTCX(path string, ifindex int, attach ebpf.AttachType, program *ebpf.Program) (link.Link, error) {
	lnk, err := link.LoadPinnedLink(path, nil)
	if err == nil {
		info, infoErr := lnk.Info()
		if infoErr != nil {
			_ = lnk.Close()
			return nil, infoErr
		}
		tcx := info.TCX()
		if tcx == nil || tcx.Ifindex != uint32(ifindex) || uint32(tcx.AttachType) != uint32(attach) {
			_ = lnk.Close()
			return nil, fmt.Errorf("pinned link %s does not target interface %d/%s", path, ifindex, attach)
		}
		if err := lnk.Update(program); err != nil {
			_ = lnk.Close()
			return nil, fmt.Errorf("update pinned link %s: %w", path, err)
		}
		return lnk, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load pinned link %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lnk, err = link.AttachTCX(link.TCXOptions{Interface: ifindex, Attach: attach, Program: program})
	if err != nil {
		return nil, err
	}
	if err := lnk.Pin(path); err != nil {
		_ = lnk.Close()
		return nil, fmt.Errorf("pin tcx link %s: %w", path, err)
	}
	return lnk, nil
}

func (m *Manager) containerPinPath(containerID string) string {
	return filepath.Join(m.pinDir, "containers", base64.RawURLEncoding.EncodeToString([]byte(containerID)))
}

// Recover local policies before refreshing the catalog, so a restart cannot
// overwrite local identity entries with seeds whose veth index is zero.
func (m *Manager) adoptContainers() error {
	dirs, err := os.ReadDir(filepath.Join(m.pinDir, "containers"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		id, err := base64.RawURLEncoding.DecodeString(dir.Name())
		if err != nil || !dir.IsDir() || len(id) == 0 {
			return fmt.Errorf("invalid pinned container directory %s", dir.Name())
		}
		path := m.containerPinPath(string(id))
		contents, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(contents) == 0 {
			// A crash after mkdir but before the first pin left no kernel state.
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		var inner *ebpf.Map
		inner, err = ebpf.LoadPinnedMap(filepath.Join(path, "conntrack"), nil)
		if err != nil {
			return fmt.Errorf("adopt conntrack for %s: %w", id, err)
		}
		template, err := m.objs.ConntrackInnerTemplate.Info()
		if err != nil {
			_ = inner.Close()
			return err
		}
		if err := (&ebpf.MapSpec{Type: template.Type, KeySize: template.KeySize, ValueSize: template.ValueSize,
			MaxEntries: template.MaxEntries, Flags: template.Flags}).Compatible(inner); err != nil {
			_ = inner.Close()
			return fmt.Errorf("adopt conntrack for %s: %w", id, err)
		}
		mapInfo, err := inner.Info()
		if err != nil {
			_ = inner.Close()
			return err
		}
		index, err := strconv.ParseUint(strings.TrimPrefix(mapInfo.Name, "ct_"), 10, 32)
		if err != nil || index == 0 {
			_ = inner.Close()
			return fmt.Errorf("conntrack map %s has invalid interface identity", mapInfo.Name)
		}
		r := &containerRuntime{containerID: string(id), innerMap: inner, ifindex: uint32(index)}
		attached := false
		m.containers[r.containerID] = r
		for _, hook := range []struct {
			name   string
			target *link.Link
		}{
			{"ingress", &r.ingressLink}, {"egress", &r.egressLink},
		} {
			lnk, err := link.LoadPinnedLink(filepath.Join(path, hook.name), nil)
			if errors.Is(err, os.ErrNotExist) {
				continue // A crash between attaching the two hooks is repairable.
			}
			if err != nil {
				return err
			}
			*hook.target = lnk
			info, err := lnk.Info()
			if err != nil {
				return err
			}
			if info.TCX() == nil {
				return fmt.Errorf("%s is not a TCX link", path)
			}
			index := info.TCX().Ifindex
			if r.ifindex != 0 && index != 0 && r.ifindex != index {
				return fmt.Errorf("container %s links target different interfaces", id)
			}
			if index != 0 {
				attached = true
			}
		}
		if !attached {
			// Both links detached because the workload's veth was removed while
			// the agent was stopped. These pins no longer protect a workload.
			if err := m.removeContainerLocked(r); err != nil {
				return err
			}
			delete(m.containers, r.containerID)
			continue
		}
		var policy firewallContainerPolicy
		if err := m.objs.ContainerPolicyMap.Lookup(r.ifindex, &policy); err != nil {
			return err
		}
		r.networkIdentity = policy.NetworkIdentity
		r.ipv4 = netip.AddrFrom4(policy.Ipv4)
		r.ipv6 = netip.AddrFrom16(policy.Ipv6)
		var matrixID ebpf.MapID
		if err := m.objs.ConntrackMatrix.Lookup(r.ifindex, &matrixID); err != nil {
			return err
		}
		info, err := inner.Info()
		if err != nil {
			return err
		}
		innerID, ok := info.ID()
		if !ok || matrixID != innerID {
			return fmt.Errorf("container %s conntrack pin differs from matrix", id)
		}
	}
	return nil
}

// Remove drops the pins, detaching programs and releasing maps once all process
// handles are closed. The agent must be stopped before explicit removal.
func Remove(interfaceName string) error {
	dir, err := pinPath(interfaceName)
	if err != nil {
		return err
	}
	lock, err := meshstate.Acquire(interfaceName)
	if err != nil {
		return err
	}
	defer lock.Close()
	return os.RemoveAll(dir)
}
