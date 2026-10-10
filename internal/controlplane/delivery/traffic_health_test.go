package delivery

import (
	"context"
	"errors"
	"net"
	"testing"

	"ebof-wg-mesh/internal/controlplane/journal"
)

func TestTrafficProbeDistinguishesServingWorkloadFromClosedListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	allocation := AllocationRecord{AllocationIPv4: "127.0.0.1", HealthyIPv4Ports: []int32{port}}
	ok, err := probeAllocationTraffic(context.Background(), allocation)
	if err != nil || !ok {
		t.Fatalf("serving workload: reachable=%v err=%v", ok, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ok, err = probeAllocationTraffic(context.Background(), allocation)
	if err != nil || ok {
		t.Fatalf("failed workload: reachable=%v err=%v", ok, err)
	}
	if ok, err := probeAllocationTraffic(context.Background(), AllocationRecord{}); ok || err == nil {
		t.Fatalf("missing probe target must leave reachability unknown: reachable=%v err=%v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probeAllocationTraffic(ctx, allocation); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe: %v", err)
	}
}

func TestReachableWorkloadSurvivesManagementSessionLoss(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	l := startLive(t)
	l.applyTestState(journal.DurableState{ClusterID: "test", LogIndex: 1,
		Services: map[string]journal.ServiceIntent{"svc": {ID: "svc"}},
		Assignments: map[string]journal.Assignment{"alloc": {
			ID: "alloc", ServiceID: "svc", AgentID: "agent", AllocationIPv4: "127.0.0.1", AllocationIPv6: "fd00::1",
			DesiredSpecRevision: 1, DesiredRolloutGeneration: 1, RolloutState: AllocationRolloutServing,
		}},
	})
	if err := l.BeginSession("agent", "s1", []string{"alloc"}, []string{"alloc"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordObservation(AllocationObservation{AgentID: "agent", SessionID: "s1", AllocationID: "alloc", RolloutGeneration: 1,
		AppliedGeneration: 1, AppliedSpecRevision: 1, Phase: "Healthy", Healthy: true,
		HealthyIPv4Ports: []int32{int32(port)}, HealthyIPv6Ports: []int32{8080},
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.EndSession("agent", "s1"); err != nil {
		t.Fatal(err)
	}
	l.ExpireForTest("agent")
	alloc := l.AllocationsByAgent("agent")[0]
	if !alloc.Healthy || len(alloc.HealthyIPv4Ports) != 1 || len(alloc.HealthyIPv6Ports) != 1 {
		t.Fatalf("management loss withdrew workload: %+v", alloc)
	}
	d := &Delivery{live: l}
	if _, ok := d.allocationsRetainedByTraffic(context.Background(), "agent")["alloc"]; !ok {
		t.Fatal("serving workload eligible for failover")
	}
	if err := l.BeginSession("agent", "s2", []string{"alloc"}, []string{"alloc"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordObservation(AllocationObservation{AgentID: "agent", SessionID: "s1", AllocationID: "alloc", RolloutGeneration: 1}); !errors.Is(err, ErrStaleAgentSession) {
		t.Fatalf("old process report accepted: %v", err)
	}
	if !l.AllocationsByAgent("agent")[0].Healthy {
		t.Fatal("new session erased traffic health")
	}
	// A newer assignment cannot borrow readiness from the previous generation.
	if rec := l.OverlayAllocation(AllocationRecord{ID: "alloc", AgentID: "agent", DesiredRolloutGeneration: 2}); rec.Healthy {
		t.Fatal("stale observation made new generation healthy")
	}
}
