package agentv1

import (
	"crypto/sha256"
	"errors"
	"math/big"
	"strconv"
)

// UsageMeterVersion names the meter definitions in docs/resource-usage.md.
// A change to what a quantity means requires a new version.
const UsageMeterVersion = "usage/v1"

// Report limits. A producer splits its backlog into reports within both.
const (
	MaxUsageRecordsPerReport = 500
	MaxUsageReportBytes      = 1 << 20
)

// Gap reasons a producer can record.
const (
	UsageGapCollectorDown   = "collector_down"
	UsageGapJournalFull     = "journal_full"
	UsageGapEpochUnobserved = "epoch_end_unobserved"
	UsageGapSampleOverrun   = "sample_overrun"
)

// UsageQualityMask covers every defined UsageQuality bit.
const UsageQualityMask = uint32(1<<10 - 1)

// HasUsageQuality reports whether quality carries flag.
func HasUsageQuality(quality uint32, flag UsageQuality) bool {
	return quality&uint32(flag) != 0
}

var errUsageInteger = errors.New("usage integer must be a canonical non-negative decimal of at most 38 digits")

// ParseUsageInteger parses a canonical decimal quantity: no sign, no
// leading zeros, at most 38 digits, matching NUMERIC(38,0).
func ParseUsageInteger(value string) (*big.Int, error) {
	if value == "" || len(value) > 38 || (len(value) > 1 && value[0] == '0') {
		return nil, errUsageInteger
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return nil, errUsageInteger
		}
	}
	n, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, errUsageInteger
	}
	return n, nil
}

// FormatUsageInteger renders n canonically. A nil n is zero.
func FormatUsageInteger(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}

// UsageRecordDigest is the identity check for a record's payload: retries
// of one sequence must carry the same digest. It hashes an explicit field
// encoding, so it does not depend on protobuf serialization details.
func UsageRecordDigest(record *UsageRecord) []byte {
	h := sha256.New()
	field := func(name, value string) {
		h.Write([]byte(name))
		h.Write([]byte{'='})
		h.Write([]byte(strconv.Quote(value)))
		h.Write([]byte{'\n'})
	}
	number := func(name string, value int64) { field(name, strconv.FormatInt(value, 10)) }
	unsigned := func(name string, value uint64) { field(name, strconv.FormatUint(value, 10)) }
	timestamp := func(name string, ts interface {
		GetSeconds() int64
		GetNanos() int32
	}) {
		field(name, strconv.FormatInt(ts.GetSeconds(), 10)+"."+strconv.FormatInt(int64(ts.GetNanos()), 10))
	}
	field("installation_id", record.GetInstallationId())
	field("producer_id", record.GetProducerId())
	unsigned("sequence", record.GetSequence())
	number("kind", int64(record.GetKind()))
	number("resource_kind", int64(record.GetResourceKind()))
	field("resource_id", record.GetResourceId())
	field("counter_epoch", record.GetCounterEpoch())
	field("boot_id", record.GetBootId())
	timestamp("start", record.GetStart())
	timestamp("end", record.GetEnd())
	number("duration_ms", record.GetDurationMs())
	field("meter_version", record.GetMeterVersion())
	unsigned("quality", uint64(record.GetQuality()))
	unsigned("cpu_microseconds", record.GetCpuMicroseconds())
	field("memory_byte_milliseconds", record.GetMemoryByteMilliseconds())
	number("memory_covered_ms", record.GetMemoryCoveredMs())
	field("volume_byte_milliseconds", record.GetVolumeByteMilliseconds())
	number("volume_covered_ms", record.GetVolumeCoveredMs())
	egress := record.GetEgress()
	unsigned("public_direct_bytes", egress.GetPublicDirectBytes())
	unsigned("public_ingress_bytes", egress.GetPublicIngressBytes())
	unsigned("private_bytes", egress.GetPrivateBytes())
	unsigned("platform_bytes", egress.GetPlatformBytes())
	unsigned("probe_bytes", egress.GetProbeBytes())
	field("gap_reason", record.GetGapReason())
	return h.Sum(nil)
}
