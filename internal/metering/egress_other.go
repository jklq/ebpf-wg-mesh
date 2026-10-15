//go:build !linux

package metering

import "errors"

// EgressSourceFor outside Linux has no kernel counters.
func EgressSourceFor(string) EgressSource { return noEgress{} }

type noEgress struct{}

func (noEgress) Read() (map[uint64]EgressCounters, uint64, error) {
	return nil, 0, errors.New("egress counters require Linux")
}
func (noEgress) Forget(uint64) error { return nil }
