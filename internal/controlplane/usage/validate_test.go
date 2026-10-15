package usage

import (
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

var testLimits = nodeLimits{cpuMillis: 4000, memoryBytes: 8 << 30}

func measurement(start time.Time, duration time.Duration) *agentv1.UsageRecord {
	return &agentv1.UsageRecord{
		Kind:         agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
		ResourceKind: agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION,
		ResourceId:   "alloc-1", CounterEpoch: "boot/1", BootId: "boot",
		Start: timestamppb.New(start), End: timestamppb.New(start.Add(duration)), DurationMs: duration.Milliseconds(),
		MeterVersion: agentv1.UsageMeterVersion, MemoryCoveredMs: duration.Milliseconds(),
	}
}

func TestValidateRecord(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	cases := []struct {
		name   string
		edit   func(*agentv1.UsageRecord)
		reject string
	}{
		{name: "valid measurement", edit: func(r *agentv1.UsageRecord) {
			r.CpuMicroseconds = 30_000_000
			r.MemoryByteMilliseconds = "6000000000"
		}},
		{name: "unknown meter version", edit: func(r *agentv1.UsageRecord) { r.MeterVersion = "usage/v0" }, reject: RejectInvalid},
		{name: "duration differs without clock step", edit: func(r *agentv1.UsageRecord) { r.DurationMs = 59_000 }, reject: RejectInvalid},
		{name: "clock step may shorten duration", edit: func(r *agentv1.UsageRecord) {
			r.DurationMs, r.MemoryCoveredMs = 5_000, 5_000
			r.Quality = uint32(agentv1.UsageQuality_USAGE_QUALITY_CLOCK_STEP)
		}},
		{name: "clock step cannot lengthen duration", edit: func(r *agentv1.UsageRecord) {
			r.DurationMs = 61_000
			r.Quality = uint32(agentv1.UsageQuality_USAGE_QUALITY_CLOCK_STEP)
		}, reject: RejectInvalid},
		{name: "end before start", edit: func(r *agentv1.UsageRecord) { r.End = timestamppb.New(start.Add(-time.Second)) }, reject: RejectInvalid},
		{name: "sub-millisecond timestamp", edit: func(r *agentv1.UsageRecord) { r.Start = timestamppb.New(start.Add(time.Microsecond)) }, reject: RejectInvalid},
		{name: "future interval", edit: func(r *agentv1.UsageRecord) {
			r.Start, r.End = timestamppb.New(now.Add(time.Hour)), timestamppb.New(now.Add(time.Hour+time.Minute))
		}, reject: RejectInvalid},
		{name: "coverage beyond duration", edit: func(r *agentv1.UsageRecord) { r.MemoryCoveredMs = 61_000 }, reject: RejectInvalid},
		{name: "non-canonical integer", edit: func(r *agentv1.UsageRecord) { r.MemoryByteMilliseconds = "007" }, reject: RejectInvalid},
		{name: "negative integer", edit: func(r *agentv1.UsageRecord) { r.MemoryByteMilliseconds = "-1" }, reject: RejectInvalid},
		{name: "unknown quality bit", edit: func(r *agentv1.UsageRecord) { r.Quality = 1 << 20 }, reject: RejectInvalid},
		{name: "allocation with volume usage", edit: func(r *agentv1.UsageRecord) { r.VolumeByteMilliseconds = "1" }, reject: RejectInvalid},
		{name: "cpu above twice node capacity", edit: func(r *agentv1.UsageRecord) { r.CpuMicroseconds = 2 * 4000 * 61_201 }, reject: RejectImplausible},
		{name: "cpu at the bound", edit: func(r *agentv1.UsageRecord) { r.CpuMicroseconds = 2 * 4000 * 61_200 }},
		{name: "memory above twice node capacity", edit: func(r *agentv1.UsageRecord) { r.MemoryByteMilliseconds = "1100000000000000" }, reject: RejectImplausible},
		{name: "gap without quantities", edit: func(r *agentv1.UsageRecord) {
			r.Kind, r.GapReason, r.MemoryCoveredMs = agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP, agentv1.UsageGapCollectorDown, 0
		}},
		{name: "gap with quantities", edit: func(r *agentv1.UsageRecord) {
			r.Kind, r.GapReason, r.MemoryCoveredMs = agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP, agentv1.UsageGapCollectorDown, 0
			r.CpuMicroseconds = 1
		}, reject: RejectInvalid},
		{name: "gap without reason", edit: func(r *agentv1.UsageRecord) {
			r.Kind, r.MemoryCoveredMs = agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP, 0
		}, reject: RejectInvalid},
		{name: "volume with egress", edit: func(r *agentv1.UsageRecord) {
			r.ResourceKind, r.MemoryCoveredMs = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME, 0
			r.Egress = &agentv1.UsageEgress{PublicDirectBytes: 1}
		}, reject: RejectInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := measurement(start, time.Minute)
			tc.edit(record)
			_, reason := validateRecord(record, now, testLimits)
			if tc.reject == "" && reason != "" {
				t.Fatalf("rejected valid record: %s", reason)
			}
			if tc.reject != "" && !strings.HasPrefix(reason, tc.reject) {
				t.Fatalf("reason = %q, want prefix %q", reason, tc.reject)
			}
		})
	}
}

func TestParseUsageIntegerIsCanonical(t *testing.T) {
	for _, value := range []string{"0", "1", "99999999999999999999999999999999999999"} {
		if _, err := agentv1.ParseUsageInteger(value); err != nil {
			t.Errorf("ParseUsageInteger(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "01", "+1", "1e3", "1.0", "100000000000000000000000000000000000000"} {
		if _, err := agentv1.ParseUsageInteger(value); err == nil {
			t.Errorf("ParseUsageInteger(%q) accepted a non-canonical value", value)
		}
	}
}
