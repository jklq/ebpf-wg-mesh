package agent

// usageObserver receives lifecycle-edge metering samples. Hooks must not
// block reconciliation on metering failures.
type usageObserver interface {
	ObserveAllocation(allocationID string, final bool)
	ObserveVolume(volumeID string, final bool)
}

type usageRuntime interface {
	SetUsageObserver(usageObserver)
}
