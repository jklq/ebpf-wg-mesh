//go:build !linux

package metering

// CgroupSource outside Linux reports no allocations.
func CgroupSource(string) AllocationSource { return emptyAllocations{} }

type emptyAllocations struct{}

func (emptyAllocations) Allocations() ([]AllocationSample, error) { return nil, nil }
func (emptyAllocations) Allocation(string) (AllocationSample, bool, error) {
	return AllocationSample{}, false, nil
}

// ReadCgroup outside Linux reads nothing.
func ReadCgroup(string) (*uint64, *uint64, uint64, error) { return nil, nil, 0, nil }
