//go:build linux

package metering

import (
	"errors"
	"os"

	"github.com/cilium/ebpf"
)

// pinnedEgress reads the firewall's pinned usage counter map. The map is
// created by the firewall manager; until it exists every read fails and
// records carry EGRESS_UNAVAILABLE.
type pinnedEgress struct{ path string }

// EgressSourceFor returns the pinned-map egress source for an interface.
func EgressSourceFor(iface string) EgressSource {
	return pinnedEgress{path: UsageCountersPinPath(iface)}
}

// UsageCountersPinPath is where the firewall pins the usage counters map.
func UsageCountersPinPath(iface string) string {
	if iface == "" {
		iface = "wg0"
	}
	return "/sys/fs/bpf/ebpf-wg-mesh/" + iface + "/maps/usage_counters"
}

func (s pinnedEgress) open() (*ebpf.Map, error) {
	if _, err := os.Stat(s.path); err != nil {
		return nil, err
	}
	return ebpf.LoadPinnedMap(s.path, nil)
}

func (s pinnedEgress) Read() (map[uint64]EgressCounters, uint64, error) {
	m, err := s.open()
	if err != nil {
		return nil, 0, err
	}
	defer m.Close()
	out := make(map[uint64]EgressCounters)
	var key uint64
	var perCPU []EgressCounters
	iter := m.Iterate()
	for iter.Next(&key, &perCPU) {
		var total EgressCounters
		for _, cpu := range perCPU {
			for i := range total {
				total[i] += cpu[i]
			}
		}
		out[key] = total
	}
	if err := iter.Err(); err != nil {
		return nil, 0, err
	}
	unattributed, err := s.readUnattributed()
	if err != nil {
		return out, 0, nil // counters still usable
	}
	return out, unattributed, nil
}

func (s pinnedEgress) readUnattributed() (uint64, error) {
	path := s.path[:len(s.path)-len("usage_counters")] + "usage_unattributed"
	m, err := ebpf.LoadPinnedMap(path, nil)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	var perCPU []uint64
	if err := m.Lookup(uint32(0), &perCPU); err != nil {
		return 0, err
	}
	var total uint64
	for _, v := range perCPU {
		total += v
	}
	return total, nil
}

func (s pinnedEgress) Forget(cgroupID uint64) error {
	m, err := s.open()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer m.Close()
	if err := m.Delete(cgroupID); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}
