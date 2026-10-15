package metering

// AllocationSample is one reading of an allocation task's cgroup. A nil
// counter could not be read; it is unknown, not zero.
type AllocationSample struct {
	AllocationID string
	// CgroupID identifies the task incarnation. The kernel never reuses it
	// within a boot, so it is the counter epoch and the egress counter token.
	CgroupID    uint64
	CPUMicros   *uint64
	MemoryBytes *uint64
}

// AllocationSource reports the allocation tasks that exist on this agent,
// including stopped tasks whose cgroups have not been cleaned up yet.
type AllocationSource interface {
	Allocations() ([]AllocationSample, error)
	Allocation(allocationID string) (AllocationSample, bool, error)
}

// VolumeSource reports volumes on this agent's disk and their used bytes.
// UsedBytes fails when the filesystem cannot be read.
type VolumeSource interface {
	Inventory() ([]string, error)
	UsedBytes(volumeID string) (int64, error)
}

// Traffic classes of allocation output, in UsageEgress field order.
const (
	classPublicDirect = iota
	classPublicIngress
	classPrivate
	classPlatform
	classProbe
	classCount
)

// EgressCounters are cumulative output bytes by traffic class.
type EgressCounters [classCount]uint64

// EgressSource reads the kernel's per-epoch output counters.
type EgressSource interface {
	// Read returns cumulative counters by cgroup ID and the cumulative bytes
	// the kernel could not attribute to an epoch counter.
	Read() (map[uint64]EgressCounters, uint64, error)
	// Forget deletes an ended epoch's counter after its final record commits.
	Forget(cgroupID uint64) error
}
