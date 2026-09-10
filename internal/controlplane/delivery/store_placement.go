package delivery

import (
	"context"
)

type placementCandidate struct {
	ID                     string
	Region                 string
	Zone                   string
	FailureDomain          string
	RuntimeCapabilities    []string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func (s *persistence) placementCandidatesQuerier(ctx context.Context, q ServiceQueryer) ([]placementCandidate, error) {
	_ = ctx
	_ = q
	if s == nil || s.live == nil {
		return nil, nil
	}
	return s.live.PlacementCandidates(), nil
}
