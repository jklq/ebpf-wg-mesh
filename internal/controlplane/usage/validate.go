package usage

import (
	"math/big"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

// Rejection reasons stored with a rejected record.
const (
	RejectInvalid            = "invalid"
	RejectImplausible        = "implausible_quantity"
	RejectUnknownAttribution = "unknown_attribution"
	RejectForeignAgent       = "foreign_agent"
	RejectBeforeAttribution  = "before_attribution"
	RejectOverlap            = "overlap"
)

const (
	// futureSkew tolerates producer clocks slightly ahead of the database.
	futureSkew = 5 * time.Minute
	// attributionSkew tolerates a record that starts slightly before the
	// control plane committed the resource's attribution.
	attributionSkew = 10 * time.Minute
	maxRecordAge    = 400 * 24 * time.Hour
	// samplingSlackMS covers CFS period carry-over and the delay between
	// reading a counter and taking its timestamp.
	samplingSlackMS = 1200
	// Physical bounds: twice the node's declared capacity, 1 PiB of volume
	// data, and 400 Gbit/s of output.
	capacityFactor      = 2
	maxVolumeBytes      = int64(1) << 50
	maxEgressBytesPerMS = 50_000_000
)

// nodeLimits are the declared physical resources of the producer's agent.
type nodeLimits struct {
	cpuMillis   int64
	memoryBytes int64
}

// quantities are the parsed, validated integers of one record.
type quantities struct {
	cpu, memory, volume *big.Int
	egress              [5]*big.Int
}

// validateRecord checks one record's encoding, interval, and physical
// plausibility. It returns the parsed quantities or a rejection detail.
func validateRecord(record *agentv1.UsageRecord, now time.Time, limits nodeLimits) (quantities, string) {
	var q quantities
	invalid := func(detail string) (quantities, string) { return q, RejectInvalid + ": " + detail }
	if record.GetMeterVersion() != agentv1.UsageMeterVersion {
		return invalid("unknown meter version")
	}
	switch record.GetResourceKind() {
	case agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION, agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME:
	default:
		return invalid("unknown resource kind")
	}
	if !validIdentifier(record.GetResourceId(), 128) || !validIdentifier(record.GetCounterEpoch(), 160) || !validIdentifier(record.GetBootId(), 64) {
		return invalid("resource, epoch, or boot identity")
	}
	if record.GetQuality()&^agentv1.UsageQualityMask != 0 {
		return invalid("unknown quality bits")
	}
	if !record.GetStart().IsValid() || !record.GetEnd().IsValid() {
		return invalid("interval timestamps")
	}
	start, end := record.GetStart().AsTime(), record.GetEnd().AsTime()
	span := end.Sub(start).Milliseconds()
	duration := record.GetDurationMs()
	if end.Before(start) || duration < 0 || start.Nanosecond()%int(time.Millisecond) != 0 || end.Nanosecond()%int(time.Millisecond) != 0 {
		return invalid("interval bounds")
	}
	if agentv1.HasUsageQuality(record.GetQuality(), agentv1.UsageQuality_USAGE_QUALITY_CLOCK_STEP) {
		if duration > span {
			return invalid("duration exceeds the stepped interval")
		}
	} else if duration != span {
		return invalid("duration differs from the interval")
	}
	if end.After(now.Add(futureSkew)) || start.Before(now.Add(-maxRecordAge)) {
		return invalid("interval outside the accepted window")
	}
	memoryCovered, volumeCovered := record.GetMemoryCoveredMs(), record.GetVolumeCoveredMs()
	if memoryCovered < 0 || volumeCovered < 0 || memoryCovered > duration || volumeCovered > duration {
		return invalid("coverage exceeds the duration")
	}
	var err error
	if q.memory, err = parseOptional(record.GetMemoryByteMilliseconds()); err != nil {
		return invalid("memory integer")
	}
	if q.volume, err = parseOptional(record.GetVolumeByteMilliseconds()); err != nil {
		return invalid("volume integer")
	}
	q.cpu = new(big.Int).SetUint64(record.GetCpuMicroseconds())
	egress := record.GetEgress()
	for i, value := range []uint64{egress.GetPublicDirectBytes(), egress.GetPublicIngressBytes(), egress.GetPrivateBytes(), egress.GetPlatformBytes(), egress.GetProbeBytes()} {
		q.egress[i] = new(big.Int).SetUint64(value)
	}
	switch record.GetKind() {
	case agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP:
		if !q.zero() || memoryCovered != 0 || volumeCovered != 0 || !validIdentifier(record.GetGapReason(), 64) {
			return invalid("gap with quantities or without a reason")
		}
		return q, ""
	case agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT:
		if record.GetGapReason() != "" {
			return invalid("measurement with a gap reason")
		}
	default:
		return invalid("unknown record kind")
	}
	if record.GetResourceKind() == agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME {
		if q.cpu.Sign() != 0 || q.memory.Sign() != 0 || memoryCovered != 0 || !zeroAll(q.egress[:]) {
			return invalid("volume record with compute or network quantities")
		}
	} else if q.volume.Sign() != 0 || volumeCovered != 0 {
		return invalid("allocation record with volume quantities")
	}
	implausible := func(detail string) (quantities, string) { return q, RejectImplausible + ": " + detail }
	if exceeds(q.cpu, capacityFactor*max(limits.cpuMillis, 1000), duration+samplingSlackMS) {
		return implausible("cpu")
	}
	if exceeds(q.memory, capacityFactor*max(limits.memoryBytes, 1<<30), memoryCovered) {
		return implausible("memory")
	}
	if exceeds(q.volume, maxVolumeBytes, volumeCovered) {
		return implausible("volume")
	}
	for _, value := range q.egress {
		if exceeds(value, maxEgressBytesPerMS, duration+samplingSlackMS) {
			return implausible("egress")
		}
	}
	return q, ""
}

func (q quantities) zero() bool {
	return q.cpu.Sign() == 0 && q.memory.Sign() == 0 && q.volume.Sign() == 0 && zeroAll(q.egress[:])
}

func zeroAll(values []*big.Int) bool {
	for _, value := range values {
		if value.Sign() != 0 {
			return false
		}
	}
	return true
}

// exceeds reports whether value > rate*milliseconds.
func exceeds(value *big.Int, rate, milliseconds int64) bool {
	limit := new(big.Int).Mul(big.NewInt(rate), big.NewInt(max(milliseconds, 0)))
	return value.Cmp(limit) > 0
}

func parseOptional(value string) (*big.Int, error) {
	if value == "" {
		return new(big.Int), nil
	}
	return agentv1.ParseUsageInteger(value)
}

func validIdentifier(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/", r))
	})
}
