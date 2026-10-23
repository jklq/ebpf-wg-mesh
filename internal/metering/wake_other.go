//go:build !linux

package metering

import "errors"

// WakeSourceFor outside Linux has no kernel counters.
func WakeSourceFor(string) WakeSource { return noWake{} }

type noWake struct{}

func (noWake) ReadWake() (map[[16]byte]uint64, error) {
	return nil, errors.New("wake counters require Linux")
}
