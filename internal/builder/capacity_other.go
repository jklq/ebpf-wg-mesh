//go:build !linux

package builder

import "math"

// Non-Linux builders only run in development, where builds execute inside
// Docker's Linux VM. The host cannot see that VM's capacity, so advertise it
// as unbounded and let Docker enforce its own limits.
func availableBuildMemory(int64) int64 { return math.MaxInt64 }
func availableBuildCPU(int64) int64    { return math.MaxInt64 }
