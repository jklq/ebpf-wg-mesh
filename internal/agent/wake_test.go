package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func sleepingDesiredService() *agentv1.DesiredService {
	return &agentv1.DesiredService{
		AllocationId:             "alloc-1",
		ServiceId:                "svc-1",
		DesiredSpecRevision:      1,
		DesiredRolloutGeneration: 1,
		Intent:                   agentv1.AllocationIntent_ALLOCATION_INTENT_SLEEP,
		DrainDeadline:            timestamppb.New(time.Now().Add(time.Minute)),
		PrivateIpv4:              "10.200.0.2",
		PrivateIpv6:              "fd00::10",
		Spec:                     &platformv1.ResolvedServiceSpec{Image: "example.com/test:1"},
	}
}

func TestContainerdRuntimeSleepStopsWorkloadAndPinsImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{
		drained: map[string]bool{"alloc-1": true},
	}
	runtime := &ContainerdRuntime{
		cfg:    config.AgentConfig{Runtime: config.RuntimeConfig{DataDir: dir, VolumesDir: filepath.Join(dir, "volumes")}},
		engine: engine,
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{
		Services: []*agentv1.DesiredService{sleepingDesiredService()},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(engine.ensured) != 0 {
		t.Fatalf("sleep started the workload: %#v", engine.ensured)
	}
	if len(engine.drainCalls) != 1 || engine.drainCalls[0] != "alloc-1" {
		t.Fatalf("drain calls = %#v", engine.drainCalls)
	}
	cond := report.Services[0]
	if cond.GetPhase() != "Sleeping" || cond.GetHealthy() {
		t.Fatalf("expected sleeping report, got %+v", cond)
	}
	if !engine.pinnedImages["example.com/test:1"] {
		t.Fatalf("sleeping image not pinned: %+v", engine.pinnedImages)
	}
	// The desired marker survives sleep so an agent restart still knows
	// the allocation exists (and must stay stopped).
	if _, err := os.Stat(filepath.Join(dir, "desired", "alloc-1.json")); err != nil {
		t.Fatalf("desired marker missing after sleep: %v", err)
	}
}

type fakeWakeSource struct {
	samples map[[16]byte]uint64
}

func (f fakeWakeSource) ReadWake() (map[[16]byte]uint64, error) {
	out := make(map[[16]byte]uint64, len(f.samples))
	for ip, packets := range f.samples {
		out[ip] = packets
	}
	return out, nil
}

func TestCollectWakeDemandsDiffsCounters(t *testing.T) {
	t.Parallel()

	runtime := &ContainerdRuntime{}
	source := fakeWakeSource{samples: map[[16]byte]uint64{}}
	runtime.SetWakeSource(source)
	svc := sleepingDesiredService()
	services := []*agentv1.DesiredService{svc}

	if demands := runtime.collectWakeDemands(services); len(demands) != 0 {
		t.Fatalf("expected no demands without traffic, got %+v", demands)
	}
	if runtime.CheckWakeDemand() {
		t.Fatal("CheckWakeDemand true without traffic")
	}

	addrs := sleepingAddresses(services)
	if len(addrs) != 2 {
		t.Fatalf("expected both families mapped, got %v", addrs)
	}
	for ip := range addrs {
		source.samples[ip] = 3
	}
	runtime.SetWakeSource(source)
	if !runtime.CheckWakeDemand() {
		t.Fatal("CheckWakeDemand false with traffic to sleeping allocation")
	}
	demands := runtime.collectWakeDemands(services)
	if len(demands) != 1 || demands[0].GetAllocationId() != "alloc-1" || demands[0].GetPackets() != 6 {
		t.Fatalf("expected one demand of 6 packets for alloc-1, got %+v", demands)
	}
	// Baseline advanced: the same counters do not re-report.
	if demands := runtime.collectWakeDemands(services); len(demands) != 0 {
		t.Fatalf("expected consumed counters, got %+v", demands)
	}
}
