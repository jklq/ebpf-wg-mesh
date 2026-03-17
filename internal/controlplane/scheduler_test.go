package controlplane

import (
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestChooseAgentLeastLoaded(t *testing.T) {
	now := time.Now().UTC()
	agentID, err := chooseAgent(
		[]agentRecord{
			{ID: "node-b", CPUMillisCapacity: 4000, MemoryMebibytesCapcity: 8192, LastSeenAt: now},
			{ID: "node-a", CPUMillisCapacity: 4000, MemoryMebibytesCapcity: 8192, LastSeenAt: now},
		},
		[]serviceRecord{
			{AllocatedAgentID: "node-b", Spec: &platformv1.ServiceSpec{CpuMillis: 500, MemoryMebibytes: 128}},
		},
		&platformv1.ServiceSpec{CpuMillis: 250, MemoryMebibytes: 64},
	)
	if err != nil {
		t.Fatalf("chooseAgent: %v", err)
	}
	if agentID != "node-a" {
		t.Fatalf("expected node-a, got %q", agentID)
	}
}

func TestChooseAgentRespectsCapacity(t *testing.T) {
	now := time.Now().UTC()
	_, err := chooseAgent(
		[]agentRecord{{ID: "node-a", CPUMillisCapacity: 500, MemoryMebibytesCapcity: 512, LastSeenAt: now}},
		[]serviceRecord{{AllocatedAgentID: "node-a", Spec: &platformv1.ServiceSpec{CpuMillis: 400, MemoryMebibytes: 256}}},
		&platformv1.ServiceSpec{CpuMillis: 200, MemoryMebibytes: 300},
	)
	if err == nil {
		t.Fatal("expected capacity failure")
	}
}
