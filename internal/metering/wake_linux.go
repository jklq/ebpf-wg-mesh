//go:build linux

package metering

import (
	"os"

	"github.com/cilium/ebpf"
)

// WakeSourceFor returns the pinned-map wake source for an interface. Until
// the firewall manager has pinned the map every read fails, like egress.
func WakeSourceFor(iface string) WakeSource {
	return pinnedWake{path: WakeCountersPinPath(iface)}
}

type pinnedWake struct{ path string }

func (s pinnedWake) ReadWake() (map[[16]byte]uint64, error) {
	if _, err := os.Stat(s.path); err != nil {
		return nil, err
	}
	m, err := ebpf.LoadPinnedMap(s.path, nil)
	if err != nil {
		return nil, err
	}
	defer m.Close()
	out := make(map[[16]byte]uint64)
	var key [16]byte
	var perCPU []uint64
	iter := m.Iterate()
	for iter.Next(&key, &perCPU) {
		var total uint64
		for _, cpu := range perCPU {
			total += cpu
		}
		if total > 0 {
			out[key] = total
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
