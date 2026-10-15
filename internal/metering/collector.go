package metering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultSampleInterval = 5 * time.Second
	defaultVolumeInterval = 60 * time.Second
	// DefaultJournalMaxBytes targets roughly seven days offline at the
	// measured record rate; operators can raise it per agent.
	DefaultJournalMaxBytes = 256 << 20
	DefaultAckedRetention  = 24 * time.Hour
)

// Config wires a Meter to its journal and measurement sources. A nil source
// disables that meter; its quality flags say the quantity is unavailable.
type Config struct {
	Dir             string
	InstallationID  string
	AgentID         string
	MaxBytes        int64
	AckedRetention  time.Duration
	SampleInterval  time.Duration
	VolumeInterval  time.Duration
	Clock           Clock
	Allocations     AllocationSource
	Volumes         VolumeSource
	Egress          EgressSource
	HostCPUs        int32
	HostMemoryBytes int64
}

// epochState is one resource counter epoch's open record plus the last
// sample folded into it. It persists as JSON in the journal.
type epochState struct {
	ResourceKind string             `json:"kind"` // "allocation" or "volume"
	ResourceID   string             `json:"resource_id"`
	Epoch        string             `json:"epoch"`
	BootID       string             `json:"boot_id"`
	CgroupID     uint64             `json:"cgroup_id,omitempty"`
	OpenStart    point              `json:"open_start"`
	Last         point              `json:"last"`
	Quality      uint32             `json:"quality"`
	LastCPU      *uint64            `json:"last_cpu,omitempty"`
	LastMem      *uint64            `json:"last_mem,omitempty"`
	LastVol      *int64             `json:"last_vol,omitempty"`
	HasEgress    bool               `json:"has_egress,omitempty"`
	LastEgress   [classCount]uint64 `json:"last_egress"`
	CPUMicros    uint64             `json:"cpu_micros"`
	MemIntegral  string             `json:"mem_integral"`
	MemCoveredMS int64              `json:"mem_covered_ms"`
	VolIntegral  string             `json:"vol_integral"`
	VolCoveredMS int64              `json:"vol_covered_ms"`
	EgressDelta  [classCount]uint64 `json:"egress_delta"`
}

func (s *epochState) clone() *epochState {
	c := *s
	if s.LastCPU != nil {
		v := *s.LastCPU
		c.LastCPU = &v
	}
	if s.LastMem != nil {
		v := *s.LastMem
		c.LastMem = &v
	}
	if s.LastVol != nil {
		v := *s.LastVol
		c.LastVol = &v
	}
	return &c
}

func allocKey(id string, cgroupID uint64) string { return fmt.Sprintf("a/%s/%d", id, cgroupID) }
func volumeKey(id string) string                 { return "v/" + id }

// Meter measures allocations and volumes, seals minute records into the
// usage journal, and delivers them to the control plane.
type Meter struct {
	cfg     Config
	journal *journal
	clock   Clock

	mu              sync.Mutex
	states          map[string]*epochState
	rateBytes       float64
	rateWindowStart time.Time
	rateWindowBytes int64

	sendMu             sync.Mutex
	send               func(*agentv1.AgentClientMessage) error
	agentID, sessionID string
	reportSeq          uint64
	sendFrom           uint64
	conflict           bool
	notify             chan struct{}
}

// Open creates or resumes a Meter's journal and checkpoints.
func Open(cfg Config) (*Meter, error) {
	if cfg.Clock == nil {
		clock, err := SystemClock()
		if err != nil {
			return nil, fmt.Errorf("system clock: %w", err)
		}
		cfg.Clock = clock
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultJournalMaxBytes
	}
	if cfg.AckedRetention <= 0 {
		cfg.AckedRetention = DefaultAckedRetention
	}
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = defaultSampleInterval
	}
	if cfg.VolumeInterval <= 0 {
		cfg.VolumeInterval = defaultVolumeInterval
	}
	j, err := openJournal(cfg.Dir, cfg.InstallationID, cfg.AgentID, cfg.MaxBytes, cfg.AckedRetention)
	if err != nil {
		return nil, err
	}
	states, err := j.states()
	if err != nil {
		_ = j.Close()
		return nil, err
	}
	return &Meter{cfg: cfg, journal: j, clock: cfg.Clock, states: states,
		sendFrom: j.meta.AckedThrough + 1, notify: make(chan struct{}, 1)}, nil
}

func (m *Meter) Close() error { return m.journal.Close() }

// ProducerID is the durable journal identity.
func (m *Meter) ProducerID() string { return m.journal.meta.ProducerID }

func (m *Meter) wake() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// Run samples until ctx ends. Tick errors are recorded as producer health,
// never fatal to the agent.
func (m *Meter) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.SampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Tick(); err != nil {
				slog.Warn("usage collection tick", "error", err)
			}
		}
	}
}

// Tick folds one sample of every resource and seals due records in one
// journal transaction. On journal exhaustion nothing folds: checkpoints stay
// frozen and recovery behaves like a collector restart.
func (m *Meter) Tick() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var allocs []AllocationSample
	if m.cfg.Allocations != nil {
		var err error
		if allocs, err = m.cfg.Allocations.Allocations(); err != nil {
			m.recordError(err)
			return err
		}
	}
	var volumeIDs []string
	if m.cfg.Volumes != nil {
		var err error
		if volumeIDs, err = m.cfg.Volumes.Inventory(); err != nil {
			m.recordError(err)
			return err
		}
	}
	egress, unattributed, egressErr := m.readEgress()
	now := m.clock.Now()
	forget, err := m.fold(now, allocs, volumeIDs, egress, unattributed, egressErr, false)
	m.afterFold(forget, err, now)
	return err
}

func (m *Meter) readEgress() (map[uint64]EgressCounters, uint64, error) {
	if m.cfg.Egress == nil {
		return nil, 0, nil
	}
	return m.cfg.Egress.Read()
}

func (m *Meter) recordError(err error) {
	_ = m.journal.update(func(t *journalTx) error {
		t.meta.LastError = err.Error()
		return nil
	})
}

func (m *Meter) afterFold(forget []uint64, err error, now Instant) {
	if errors.Is(err, errJournalFull) {
		_ = m.journal.update(func(t *journalTx) error {
			if t.meta.ExhaustedSinceMS == 0 {
				t.meta.ExhaustedSinceMS = now.Wall.UnixMilli()
			}
			t.meta.LastError = errJournalFull.Error()
			return nil
		})
		return
	}
	if err != nil {
		return
	}
	if m.journal.meta.ExhaustedSinceMS != 0 {
		_ = m.journal.update(func(t *journalTx) error {
			t.meta.ExhaustedSinceMS = 0
			return nil
		})
	}
	if m.cfg.Egress != nil {
		for _, id := range forget {
			if ferr := m.cfg.Egress.Forget(id); ferr != nil {
				slog.Warn("forget egress counter", "cgroup_id", id, "error", ferr)
			}
		}
	}
	m.wake()
}

// fold performs one atomic sample fold. States are deep-copied first so a
// failed transaction leaves the in-memory checkpoints untouched.
func (m *Meter) fold(now Instant, allocs []AllocationSample, volumeIDs []string,
	egress map[uint64]EgressCounters, unattributed uint64, egressErr error, volumesOnly bool) ([]uint64, error) {
	next := make(map[string]*epochState, len(m.states))
	for k, v := range m.states {
		next[k] = v.clone()
	}
	var forget []uint64
	err := m.journal.update(func(t *journalTx) error {
		p := t.meta.Timeline.at(now)
		bootStart := point{Boot: now.Boot, Mono: 0, MS: p.MS - p.Mono/int64(time.Millisecond), Steps: p.Steps, Skewed: p.Skewed}
		scanStart := bootStart
		if t.meta.LastTick != nil && t.meta.LastTick.Boot == now.Boot {
			scanStart = *t.meta.LastTick
		}
		f := &folder{m: m, t: t, p: p, states: next, egress: egress, egressErr: egressErr}
		// End epochs from an earlier boot with a gap to the boot start.
		for key, st := range next {
			if st.BootID == now.Boot {
				continue
			}
			if err := f.gapFrom(st, bootStart, agentv1.UsageGapCollectorDown); err != nil {
				return err
			}
			if err := t.deleteState(key); err != nil {
				return err
			}
			delete(next, key)
			if st.CgroupID != 0 {
				forget = append(forget, st.CgroupID)
			}
		}
		if !volumesOnly {
			if err := f.foldAllocations(allocs, scanStart, &forget); err != nil {
				return err
			}
		}
		if err := f.foldVolumes(volumeIDs, scanStart); err != nil {
			return err
		}
		t.meta.LastTick = &p
		t.meta.Unattributed = unattributed
		t.meta.LastError = ""
		sealed := p.MS
		for _, st := range next {
			if st.OpenStart.MS < sealed {
				sealed = st.OpenStart.MS
			}
		}
		t.meta.SealedThroughMS = sealed
		t.pruneAcknowledged(false)
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.states = next
	return forget, nil
}

type folder struct {
	m         *Meter
	t         *journalTx
	p         point
	states    map[string]*epochState
	egress    map[uint64]EgressCounters
	egressErr error
}

func (f *folder) foldAllocations(samples []AllocationSample, scanStart point, forget *[]uint64) error {
	seen := make(map[string]bool, len(samples))
	sort.Slice(samples, func(i, j int) bool { return samples[i].AllocationID < samples[j].AllocationID })
	for _, s := range samples {
		if s.AllocationID == "" || s.CgroupID == 0 {
			continue
		}
		key := allocKey(s.AllocationID, s.CgroupID)
		seen[key] = true
		st, ok := f.states[key]
		if !ok {
			st = f.newAllocState(s, scanStart)
			f.states[key] = st
			if err := f.t.putState(key, st); err != nil {
				return err
			}
			continue
		}
		if err := f.foldAllocSample(key, st, s); err != nil {
			return err
		}
	}
	// End allocation epochs the scan no longer sees.
	var ended []string
	for key, st := range f.states {
		if st.ResourceKind == "allocation" && !seen[key] {
			ended = append(ended, key)
		}
	}
	sort.Strings(ended)
	for _, key := range ended {
		st := f.states[key]
		if err := f.endUnobserved(key, st); err != nil {
			return err
		}
		if st.CgroupID != 0 {
			*forget = append(*forget, st.CgroupID)
		}
	}
	return nil
}

func (f *folder) newAllocState(s AllocationSample, start point) *epochState {
	st := &epochState{ResourceKind: "allocation", ResourceID: s.AllocationID,
		Epoch: fmt.Sprintf("%s/%d", f.p.Boot, s.CgroupID), BootID: f.p.Boot, CgroupID: s.CgroupID,
		OpenStart: start, Last: f.p, MemIntegral: "0", VolIntegral: "0",
		Quality: uint32(agentv1.UsageQuality_USAGE_QUALITY_EPOCH_START)}
	if start.MS < f.p.MS {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_UNCERTAIN_TIMING)
	}
	st.LastCPU, st.LastMem = cloneU64(s.CPUMicros), cloneU64(s.MemoryBytes)
	if c, ok := f.egress[s.CgroupID]; ok && f.egressErr == nil {
		st.HasEgress, st.LastEgress = true, c
	}
	return st
}

func (f *folder) foldAllocSample(key string, st *epochState, s AllocationSample) error {
	duration, clockQ := interval(st.Last, f.p)
	st.Quality |= clockQ
	periodMS := f.m.cfg.SampleInterval.Milliseconds()
	overrun := duration > 2*periodMS
	if s.CPUMicros != nil && st.LastCPU != nil && *s.CPUMicros >= *st.LastCPU {
		st.CPUMicros += *s.CPUMicros - *st.LastCPU
	} else if s.CPUMicros == nil || st.LastCPU == nil {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_CPU_UNAVAILABLE)
	} else {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_UNCERTAIN_TIMING)
	}
	covered := min(max(duration, 0), 2*periodMS)
	if st.LastMem != nil {
		st.MemIntegral = addProduct(st.MemIntegral, *st.LastMem, covered)
		st.MemCoveredMS += covered
	} else {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_MEMORY_UNAVAILABLE)
	}
	if f.m.cfg.Egress == nil || f.egressErr != nil {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_EGRESS_UNAVAILABLE)
	} else if c, ok := f.egress[st.CgroupID]; ok {
		if st.HasEgress {
			for i := range c {
				if c[i] >= st.LastEgress[i] {
					st.EgressDelta[i] += c[i] - st.LastEgress[i]
				}
			}
		}
		st.HasEgress, st.LastEgress = true, c
	} else {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_EGRESS_UNAVAILABLE)
	}
	st.LastCPU, st.LastMem, st.Last = cloneU64(s.CPUMicros), cloneU64(s.MemoryBytes), f.p
	if overrun {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_UNCERTAIN_TIMING)
		return f.sealAt(key, st, f.p, false)
	}
	if crossedMinute(st.OpenStart.MS, f.p.MS) {
		return f.sealAt(key, st, f.p, false)
	}
	return f.t.putState(key, st)
}

func (f *folder) foldVolumes(ids []string, scanStart point) error {
	if f.m.cfg.Volumes == nil {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	sort.Strings(ids)
	for _, id := range ids {
		key := volumeKey(id)
		seen[key] = true
		st, ok := f.states[key]
		if ok && f.p.MS-st.Last.MS < f.m.cfg.VolumeInterval.Milliseconds() {
			continue // not due; gauge holds from the last sample
		}
		used, err := f.m.cfg.Volumes.UsedBytes(id)
		if !ok {
			st = &epochState{ResourceKind: "volume", ResourceID: id, Epoch: "volume", BootID: f.p.Boot,
				OpenStart: scanStart, Last: f.p, MemIntegral: "0", VolIntegral: "0",
				Quality: uint32(agentv1.UsageQuality_USAGE_QUALITY_EPOCH_START)}
			if err == nil {
				st.LastVol = &used
			} else {
				st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_VOLUME_UNKNOWN)
			}
			f.states[key] = st
			if err := f.t.putState(key, st); err != nil {
				return err
			}
			continue
		}
		if err := f.foldVolumeSample(key, st, used, err); err != nil {
			return err
		}
	}
	var ended []string
	for key, st := range f.states {
		if st.ResourceKind == "volume" && !seen[key] {
			ended = append(ended, key)
		}
	}
	sort.Strings(ended)
	for _, key := range ended {
		if err := f.endUnobserved(key, f.states[key]); err != nil {
			return err
		}
	}
	return nil
}

func (f *folder) foldVolumeSample(key string, st *epochState, used int64, usedErr error) error {
	duration, clockQ := interval(st.Last, f.p)
	st.Quality |= clockQ
	covered := min(max(duration, 0), 2*f.m.cfg.VolumeInterval.Milliseconds())
	if st.LastVol != nil {
		st.VolIntegral = addProduct(st.VolIntegral, uint64(max(*st.LastVol, 0)), covered)
		st.VolCoveredMS += covered
	} else {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_VOLUME_UNKNOWN)
	}
	if duration > 2*f.m.cfg.VolumeInterval.Milliseconds() {
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_UNCERTAIN_TIMING)
	}
	if usedErr == nil {
		st.LastVol = &used
	} else {
		st.LastVol = nil
		st.Quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_VOLUME_UNKNOWN)
	}
	st.Last = f.p
	if crossedMinute(st.OpenStart.MS, f.p.MS) {
		return f.sealAt(key, st, f.p, false)
	}
	return f.t.putState(key, st)
}

// sealAt closes the open record at end and starts a fresh open record there,
// unless final, which deletes the checkpoint.
func (f *folder) sealAt(key string, st *epochState, end point, final bool) error {
	duration, clockQ := interval(st.OpenStart, end)
	if duration > 0 {
		quality := st.Quality | clockQ
		if final {
			quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_EPOCH_END)
		}
		if (st.ResourceKind == "allocation" && st.MemCoveredMS < duration) ||
			(st.ResourceKind == "volume" && st.VolCoveredMS < duration) {
			quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_GAUGE_GAP)
		}
		rec := &agentv1.UsageRecord{
			Kind:       agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
			ResourceId: st.ResourceID, CounterEpoch: st.Epoch, BootId: st.BootID,
			Start:      timestamppb.New(time.UnixMilli(st.OpenStart.MS).UTC()),
			End:        timestamppb.New(time.UnixMilli(end.MS).UTC()),
			DurationMs: duration, Quality: quality, CpuMicroseconds: st.CPUMicros,
			MemoryByteMilliseconds: nonzero(st.MemIntegral), MemoryCoveredMs: st.MemCoveredMS,
			VolumeByteMilliseconds: nonzero(st.VolIntegral), VolumeCoveredMs: st.VolCoveredMS,
			Egress: &agentv1.UsageEgress{PublicDirectBytes: st.EgressDelta[0], PublicIngressBytes: st.EgressDelta[1],
				PrivateBytes: st.EgressDelta[2], PlatformBytes: st.EgressDelta[3], ProbeBytes: st.EgressDelta[4]},
		}
		if st.ResourceKind == "volume" {
			rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME
		} else {
			rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION
		}
		if err := f.t.append(rec); err != nil {
			return err
		}
	}
	if final {
		delete(f.states, key)
		return f.t.deleteState(key)
	}
	st.OpenStart, st.Quality = end, 0
	st.CPUMicros, st.MemIntegral, st.MemCoveredMS = 0, "0", 0
	st.VolIntegral, st.VolCoveredMS, st.EgressDelta = "0", 0, [classCount]uint64{}
	return f.t.putState(key, st)
}

// gapFrom discards the open partial record and seals a GAP to end.
func (f *folder) gapFrom(st *epochState, end point, reason string) error {
	if end.MS <= st.OpenStart.MS {
		return nil
	}
	rec := &agentv1.UsageRecord{Kind: agentv1.UsageRecordKind_USAGE_RECORD_KIND_GAP,
		ResourceId: st.ResourceID, CounterEpoch: st.Epoch, BootId: st.BootID,
		Start:      timestamppb.New(time.UnixMilli(st.OpenStart.MS).UTC()),
		End:        timestamppb.New(time.UnixMilli(end.MS).UTC()),
		DurationMs: end.MS - st.OpenStart.MS, GapReason: reason, Egress: &agentv1.UsageEgress{}}
	if st.ResourceKind == "volume" {
		rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME
	} else {
		rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION
	}
	return f.t.append(rec)
}

// endUnobserved seals folded quantities at the last sample, then a gap for
// the unobserved remainder: final counters were lost.
func (f *folder) endUnobserved(key string, st *epochState) error {
	if st.Last.MS > st.OpenStart.MS {
		if err := f.sealKeep(st, st.Last, true); err != nil {
			return err
		}
		st.OpenStart = st.Last
	}
	if err := f.gapFrom(st, f.p, agentv1.UsageGapEpochUnobserved); err != nil {
		return err
	}
	delete(f.states, key)
	return f.t.deleteState(key)
}

func (f *folder) sealKeep(st *epochState, end point, epochEnd bool) error {
	duration, clockQ := interval(st.OpenStart, end)
	if duration <= 0 {
		return nil
	}
	quality := st.Quality | clockQ
	if epochEnd {
		quality |= uint32(agentv1.UsageQuality_USAGE_QUALITY_EPOCH_END)
	}
	rec := &agentv1.UsageRecord{Kind: agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
		ResourceId: st.ResourceID, CounterEpoch: st.Epoch, BootId: st.BootID,
		Start:      timestamppb.New(time.UnixMilli(st.OpenStart.MS).UTC()),
		End:        timestamppb.New(time.UnixMilli(end.MS).UTC()),
		DurationMs: duration, Quality: quality, CpuMicroseconds: st.CPUMicros,
		MemoryByteMilliseconds: nonzero(st.MemIntegral), MemoryCoveredMs: st.MemCoveredMS,
		VolumeByteMilliseconds: nonzero(st.VolIntegral), VolumeCoveredMs: st.VolCoveredMS,
		Egress: &agentv1.UsageEgress{PublicDirectBytes: st.EgressDelta[0], PublicIngressBytes: st.EgressDelta[1],
			PrivateBytes: st.EgressDelta[2], PlatformBytes: st.EgressDelta[3], ProbeBytes: st.EgressDelta[4]}}
	if st.ResourceKind == "volume" {
		rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_VOLUME
	} else {
		rec.ResourceKind = agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION
	}
	return f.t.append(rec)
}

func crossedMinute(startMS, endMS int64) bool { return endMS/60000 > startMS/60000 }

func cloneU64(v *uint64) *uint64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func nonzero(s string) string {
	if s == "" || s == "0" {
		return ""
	}
	return s
}

func addProduct(current string, value uint64, ms int64) string {
	n, _ := new(big.Int).SetString(current, 10)
	if n == nil {
		n = new(big.Int)
	}
	term := new(big.Int).Mul(new(big.Int).SetUint64(value), big.NewInt(ms))
	return n.Add(n, term).String()
}

// ObserveAllocation folds one allocation at a lifecycle edge. A non-final
// hook starts a new epoch at the hook sample; a final hook seals it.
func (m *Meter) ObserveAllocation(allocationID string, final bool) {
	if m.cfg.Allocations == nil {
		return
	}
	sample, found, err := m.cfg.Allocations.Allocation(allocationID)
	egress, unattributed, egressErr := m.readEgress()
	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*epochState, len(m.states))
	for k, v := range m.states {
		next[k] = v.clone()
	}
	var forget []uint64
	txErr := m.journal.update(func(t *journalTx) error {
		p := t.meta.Timeline.at(now)
		f := &folder{m: m, t: t, p: p, states: next, egress: egress, egressErr: egressErr}
		if err == nil && found {
			key := allocKey(sample.AllocationID, sample.CgroupID)
			st, ok := next[key]
			if !ok {
				next[key] = f.newAllocState(sample, p)
				if err := t.putState(key, next[key]); err != nil {
					return err
				}
				if final {
					delete(next, key)
					return t.deleteState(key)
				}
				return nil
			}
			if err := f.foldAllocSampleNoSeal(st, sample); err != nil {
				return err
			}
			if final {
				forget = append(forget, st.CgroupID)
				return f.sealAt(key, st, p, true)
			}
			return t.putState(key, st)
		}
		if final {
			for key, st := range next {
				if st.ResourceKind == "allocation" && st.ResourceID == allocationID {
					if err := f.endUnobserved(key, st); err != nil {
						return err
					}
					forget = append(forget, st.CgroupID)
				}
			}
		}
		_ = unattributed
		return nil
	})
	if txErr == nil {
		m.states = next
	}
	m.afterFold(forget, txErr, now)
}

func (f *folder) foldAllocSampleNoSeal(st *epochState, s AllocationSample) error {
	duration, clockQ := interval(st.Last, f.p)
	st.Quality |= clockQ
	if s.CPUMicros != nil && st.LastCPU != nil && *s.CPUMicros >= *st.LastCPU {
		st.CPUMicros += *s.CPUMicros - *st.LastCPU
	}
	covered := min(max(duration, 0), 2*f.m.cfg.SampleInterval.Milliseconds())
	if st.LastMem != nil {
		st.MemIntegral = addProduct(st.MemIntegral, *st.LastMem, covered)
		st.MemCoveredMS += covered
	}
	if c, ok := f.egress[st.CgroupID]; ok && f.egressErr == nil {
		if st.HasEgress {
			for i := range c {
				if c[i] >= st.LastEgress[i] {
					st.EgressDelta[i] += c[i] - st.LastEgress[i]
				}
			}
		}
		st.HasEgress, st.LastEgress = true, c
	}
	st.LastCPU, st.LastMem, st.Last = cloneU64(s.CPUMicros), cloneU64(s.MemoryBytes), f.p
	return nil
}

// ObserveVolume folds one volume at a lifecycle edge.
func (m *Meter) ObserveVolume(volumeID string, final bool) {
	if m.cfg.Volumes == nil {
		return
	}
	used, usedErr := m.cfg.Volumes.UsedBytes(volumeID)
	now := m.clock.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*epochState, len(m.states))
	for k, v := range m.states {
		next[k] = v.clone()
	}
	txErr := m.journal.update(func(t *journalTx) error {
		p := t.meta.Timeline.at(now)
		f := &folder{m: m, t: t, p: p, states: next}
		key := volumeKey(volumeID)
		st, ok := next[key]
		if !ok {
			if final {
				return nil
			}
			st = &epochState{ResourceKind: "volume", ResourceID: volumeID, Epoch: "volume", BootID: p.Boot,
				OpenStart: p, Last: p, MemIntegral: "0", VolIntegral: "0",
				Quality: uint32(agentv1.UsageQuality_USAGE_QUALITY_EPOCH_START)}
			if usedErr == nil {
				st.LastVol = &used
			}
			next[key] = st
			return t.putState(key, st)
		}
		duration, clockQ := interval(st.Last, p)
		st.Quality |= clockQ
		covered := min(max(duration, 0), 2*m.cfg.VolumeInterval.Milliseconds())
		if st.LastVol != nil {
			st.VolIntegral = addProduct(st.VolIntegral, uint64(max(*st.LastVol, 0)), covered)
			st.VolCoveredMS += covered
		}
		st.Last = p
		if final {
			return f.sealAt(key, st, p, true)
		}
		if usedErr == nil {
			st.LastVol = &used
		}
		return t.putState(key, st)
	})
	if txErr == nil {
		m.states = next
	}
	m.afterFold(nil, txErr, now)
}
