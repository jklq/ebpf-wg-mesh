package main

import (
	"context"
	"testing"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type listAgentsStub struct {
	platformv1.PlatformServiceClient
	addr   string
	err    error
	events *[]string
}

func (s listAgentsStub) ListAgents(context.Context, *emptypb.Empty, ...grpc.CallOption) (*platformv1.ListAgentsResponse, error) {
	*s.events = append(*s.events, "probe:"+s.addr)
	if s.err != nil {
		return nil, s.err
	}
	return &platformv1.ListAgentsResponse{}, nil
}

func TestCheckSingleOwner(t *testing.T) {
	old := stressLease{Holder: "a", Address: "192.0.2.1:9443", Token: 1}
	next := stressLease{Holder: "b", Address: "192.0.2.1:9444", Token: 2}

	if err := checkSingleOwner([]string{next.Address}, next, old); err != nil {
		t.Fatalf("post-probe holder match: %v", err)
	}
	if err := checkSingleOwner([]string{old.Address, next.Address}, next, old); err == nil {
		t.Fatal("split brain accepted")
	}
	if err := checkSingleOwner([]string{old.Address}, next, old); err == nil {
		t.Fatal("serving replica must match the post-probe lease, not the stale pre-probe holder")
	}
	if err := checkSingleOwner([]string{next.Address}, next, old); err != nil {
		t.Fatalf("stale pre-probe holder must not fail a matching post-probe owner: %v", err)
	}
}

func TestAssertSingleOwnerReadsLeaseAfterProbes(t *testing.T) {
	primary := "192.0.2.1:" + primaryControlPlanePort
	replica := "192.0.2.1:" + replicaControlPlanePort
	var events []string
	clients := []platformv1.PlatformServiceClient{
		listAgentsStub{addr: primary, err: status.Error(codes.FailedPrecondition, "not the live owner; reconnect at "+replica), events: &events},
		listAgentsStub{addr: replica, events: &events},
	}
	baseline := stressLease{Holder: "old", Address: primary, Token: 1}
	err := assertSingleOwnerWith(context.Background(), stressOptions{Recovery: time.Second, RPCTimeout: time.Second}, clients, []string{primary, replica}, baseline, func(context.Context) (stressLease, error) {
		events = append(events, "lease")
		return stressLease{Holder: "new", Address: replica, Token: 2}, nil
	})
	if err != nil {
		t.Fatalf("failover between baseline and probes: %v", err)
	}
	if len(events) < 3 || events[0] == "lease" || events[len(events)-1] != "lease" {
		t.Fatalf("lease must be read after probes, got %v", events)
	}
}

func TestStressLeaseKeepsHolderIdentitySeparateFromAddress(t *testing.T) {
	lease, err := parseStressLease("holder_id\tfencing_token\tadvertise_addr\n2e4393ca-e97d-4fa7-b3b2-585a2a333360\t7\t192.0.2.1:9444\n")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "2e4393ca-e97d-4fa7-b3b2-585a2a333360" || lease.Address != "192.0.2.1:9444" || lease.Token != 7 {
		t.Fatalf("lease=%+v", lease)
	}
	if _, err := parseStressLease("holder_id\tfencing_token\tadvertise_addr\n"); err == nil {
		t.Fatal("missing live lease accepted")
	}
}
