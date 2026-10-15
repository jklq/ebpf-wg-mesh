//go:build integration

package controlplane

import (
	"context"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/usage"
	"ebof-wg-mesh/internal/metering"
)

type e2eClock struct{ now metering.Instant }

func (c *e2eClock) Now() metering.Instant { return c.now }
func (c *e2eClock) advance(d time.Duration) {
	c.now.Mono += d
	c.now.Wall = c.now.Wall.Add(d)
}

type e2eAllocs struct{ samples []metering.AllocationSample }

func (f *e2eAllocs) Allocations() ([]metering.AllocationSample, error) { return f.samples, nil }
func (f *e2eAllocs) Allocation(id string) (metering.AllocationSample, bool, error) {
	for _, s := range f.samples {
		if s.AllocationID == id {
			return s, true, nil
		}
	}
	return metering.AllocationSample{}, false, nil
}

// TestUsageEndToEndJournalToQuery drives the real agent journal and delivery
// loop into PostgreSQL ingestion and back out through ProjectUsage.
func TestUsageEndToEndJournalToQuery(t *testing.T) {
	f := newUsageFixture(t)
	ctx := context.Background()
	// The fixture's journal producer is the meter's own; align ingest to it
	// by delivering through f.usage with the meter's producer ID.
	cpu, mem := uint64(0), uint64(100<<20)
	clock := &e2eClock{now: metering.Instant{Boot: "boot-e2e", Mono: 5 * time.Second,
		Wall: time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)}}
	allocs := &e2eAllocs{samples: []metering.AllocationSample{
		{AllocationID: f.allocation, CgroupID: 4242, CPUMicros: &cpu, MemoryBytes: &mem}}}
	meter, err := metering.Open(metering.Config{
		Dir: t.TempDir(), InstallationID: usageTestInstallation, AgentID: f.agent,
		Clock: clock, Allocations: allocs, SampleInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer meter.Close()
	// Warm-up tick sets LastTick, then a minute of samples seals one record.
	saved := allocs.samples
	allocs.samples = nil
	if err := meter.Tick(); err != nil {
		t.Fatal(err)
	}
	allocs.samples = saved
	if err := meter.Tick(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		clock.advance(5 * time.Second)
		cpu += 2_500_000
		if err := meter.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	producer := f.producer(t)
	deliverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	acked := make(chan struct{}, 1)
	meter.Attach(f.agent, "session-e2e", func(msg *agentv1.AgentClientMessage) error {
		report := msg.GetUsageReport()
		if report == nil || len(report.GetRecords()) == 0 {
			return nil
		}
		ack, err := f.usage.Ingest(ctx, producer, report)
		if err != nil {
			return err
		}
		meter.Acknowledge(ack)
		select {
		case acked <- struct{}{}:
		default:
		}
		return nil
	})
	go meter.Deliver(deliverCtx)
	select {
	case <-acked:
	case <-time.After(15 * time.Second):
		t.Fatal("usage report was not delivered and acknowledged")
	}
	result := f.query(t, usage.Query{
		Start: clock.now.Wall.Add(-time.Hour), End: clock.now.Wall.Add(time.Hour),
		Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_MINUTE,
	})
	// The epoch starts on a minute boundary; 12 samples of 2.5s CPU seal
	// exactly one 60s record with 30s of CPU time.
	if got := result.GetTotal().GetCpuMicroseconds(); got != "30000000" {
		t.Fatalf("total cpu = %s, want 30000000", got)
	}
	if result.GetTotal().GetMeasuredMilliseconds() == 0 {
		t.Fatal("no measured time in query result")
	}
}
