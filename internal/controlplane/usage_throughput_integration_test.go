//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/usage"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// BenchmarkUsageIngest measures full 500-record report ingestion across
// 1,000 allocations (1.44M records/day at that scale). It is a test so it
// can reuse the integration fixture; run with -v and read the log line,
// or record it under docs/benchmarks.
func TestUsageIngestThroughput1000Allocations(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput measurement")
	}
	f := newUsageFixture(t)
	ctx := context.Background()
	const allocations = 1000
	for i := 0; i < allocations; i++ {
		id := fmt.Sprintf("bench-alloc-%d", i)
		if _, err := f.store.db.Exec(`INSERT INTO usage_attributions
			(resource_kind, resource_id, agent_id, project_id, environment_id, service_id, environment_name, resource_name, created_at)
			VALUES ('allocation', $1, $2, $3, $4, $5, 'bench', $1, now() - interval '400 days')`,
			id, f.agent, f.project, f.environment, f.service); err != nil {
			t.Fatal(err)
		}
	}
	producer := f.producer(t)
	start := time.Now().UTC().Truncate(time.Minute).Add(-30 * 24 * time.Hour)
	const reports = 20
	ingestStart := time.Now()
	for n := 0; n < reports; n++ {
		records := make([]*agentv1.UsageRecord, 0, 500)
		for i := 0; i < 500; i++ {
			seq := uint64(n*500 + i + 1)
			at := start.Add(time.Duration(seq) * time.Minute)
			records = append(records, &agentv1.UsageRecord{
				InstallationId: usageTestInstallation, ProducerId: f.producerID, Sequence: seq,
				Kind:         agentv1.UsageRecordKind_USAGE_RECORD_KIND_MEASUREMENT,
				ResourceKind: agentv1.UsageResourceKind_USAGE_RESOURCE_KIND_ALLOCATION,
				ResourceId:   fmt.Sprintf("bench-alloc-%d", int(seq-1)%allocations),
				CounterEpoch: "boot-1/1", BootId: "boot-1",
				Start: timestamppb.New(at), End: timestamppb.New(at.Add(time.Minute)), DurationMs: 60_000,
				MeterVersion: agentv1.UsageMeterVersion, CpuMicroseconds: 30_000_000,
				MemoryByteMilliseconds: "6291456000", MemoryCoveredMs: 60_000,
				Egress: &agentv1.UsageEgress{PublicDirectBytes: 1024},
			})
		}
		if _, err := f.usage.Ingest(ctx, producer, &agentv1.UsageReport{
			AgentId: f.agent, InstallationId: usageTestInstallation, ProducerId: f.producerID, Records: records,
		}); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(ingestStart)
	t.Logf("ingest: %d records in %s (%.0f records/s, %s/record)",
		reports*500, elapsed, float64(reports*500)/elapsed.Seconds(), elapsed/(reports*500))

	end := time.Now().UTC()
	queryStart := time.Now()
	result, err := f.usage.ProjectUsage(ctx, usage.ProjectAccess(authzProject(t, f)), usage.Query{
		Start: end.Add(-30 * 24 * time.Hour), End: end,
		Resolution: platformv1.UsageResolution_USAGE_RESOLUTION_HOUR,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("query 30d hourly: %s, %d buckets, %d resources",
		time.Since(queryStart), len(result.GetBuckets()), len(result.GetResources()))
}
