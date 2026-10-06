//go:build !linux

package builder

// Non-Linux development needs an explicit Linux builder to advertise capacity.
// Fail closed rather than promise memory we cannot measure.
func availableBuildMemory(int64) int64 { return 0 }
func availableBuildCPU(int64) int64    { return 0 }
