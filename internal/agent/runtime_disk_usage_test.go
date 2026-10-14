package agent

import (
	"errors"
	"testing"
	"time"
)

func TestDiskUsageCacheBoundsScansAndForgetsRemovedSnapshots(t *testing.T) {
	var cache diskUsageCache
	now := time.Unix(100, 0)
	calls := 0
	written := int64(100)
	var failure error
	measure := func() (int64, error) { calls++; return written, failure }
	check := func(id string, at time.Time, want int64, wantCalls int) {
		t.Helper()
		got, err := cache.usage(id, at, measure)
		if got != want || !errors.Is(err, failure) || calls != wantCalls {
			t.Fatalf("usage = %d, %v; scans = %d; want %d, %v, %d", got, err, calls, want, failure, wantCalls)
		}
	}
	check("one", now, 100, 1)
	written = 200
	check("one", now.Add(diskUsageInterval-time.Nanosecond), 100, 1)
	check("two", now, 200, 2)
	check("one", now.Add(diskUsageInterval), 200, 3)
	failure = errors.New("snapshot service unavailable")
	check("one", now.Add(2*diskUsageInterval), 200, 4)
	check("one", now.Add(2*diskUsageInterval+time.Second), 200, 4)
	failure = nil
	cache.remove("one")
	written = 0
	check("one", now.Add(2*diskUsageInterval+time.Second), 0, 5)
}
