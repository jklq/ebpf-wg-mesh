package main

import (
	"context"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/delivery"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStressScheduleReplaysAndCoversFaults(t *testing.T) {
	options := stressOptions{Seed: 42, Stages: len(stressFaultKinds) + 1, ServicesPerStage: 4, MaxConcurrency: 16}
	a := stressSchedule(options, []string{"agent-b", "agent-a"})
	b := stressSchedule(options, []string{"agent-a", "agent-b"})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("agent enumeration changed seeded plan")
	}
	faults := map[string]bool{}
	compounded := false
	for i, stage := range a {
		if stage.Number != i+1 {
			t.Fatalf("stage numbering wrong: %+v", stage)
		}
		for _, fault := range stage.Faults {
			faults[fault.Kind] = true
			if fault.Target == "" {
				t.Fatalf("fault without target: %+v", fault)
			}
		}
		if len(stage.Faults) > 1 {
			compounded = true
			if stage.Faults[0].Kind == stage.Faults[1].Kind {
				t.Fatalf("stage %d compounded the same fault twice", stage.Number)
			}
		}
		if stage.Services != 4*(i+1) || stage.Concurrency > 16 {
			t.Fatalf("invalid ramp: %+v", stage)
		}
		seen := make(map[int]bool)
		for _, index := range stage.Updates {
			if index < 0 || index >= stage.Services || seen[index] {
				t.Fatalf("invalid mutation schedule: %v", stage.Updates)
			}
			seen[index] = true
		}
		if len(seen) != stage.Services {
			t.Fatal("missing service mutations")
		}
	}
	for _, kind := range stressFaultKinds {
		if !faults[kind.Name] {
			t.Fatalf("fault %s was never scheduled across %d stages", kind.Name, options.Stages)
		}
	}
	if !compounded {
		t.Fatal("campaign never compounded two faults")
	}
	options.Seed++
	if reflect.DeepEqual(a, stressSchedule(options, []string{"agent-a", "agent-b"})) {
		t.Fatal("seed did not change schedule")
	}
}

func TestStressFaultStepsHealExactlyWhatTheyInject(t *testing.T) {
	hosts := map[string]hostInfo{
		"controlplane": {Name: "controlplane", PublicIPv4: "10.0.0.1"},
		"agent-a":      {Name: "agent-a", PublicIPv4: "10.0.0.2"},
	}
	steps, err := stressFaultSteps(hosts, "ebpf-wg-mesh-controlplane", []stressFault{
		{Kind: "database-restart", Target: "controlplane"},
		{Kind: "agent-partition", Target: "agent-a"},
		{Kind: "agent-wg-partition", Target: "agent-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("got %d steps", len(steps))
	}
	for _, step := range steps {
		if step.Inject == "" || step.Heal == nil || step.Heal() == "" {
			t.Fatalf("empty command in %+v", step)
		}
		if step.Host == "" {
			t.Fatalf("unresolved host in %+v", step)
		}
	}
	if steps[1].Host != "agent-a" || steps[1].Heal()[:12] != "iptables -D " {
		t.Fatalf("partition heal does not remove injected rules: %q", steps[1].Heal())
	}
	if _, err := stressFaultSteps(hosts, "svc", []stressFault{{Kind: "not-a-fault"}}); err == nil {
		t.Fatal("unknown fault accepted")
	}
}

func TestStressFaultDataPlaneSafety(t *testing.T) {
	if stressFaultIsDataPlaneSafe("agent-kill") || stressFaultIsDataPlaneSafe("agent-wg-partition") {
		t.Fatal("agent-targeting faults must allow data-plane disruption")
	}
	if stressFaultIsDataPlaneSafe("not-a-fault") || stressFaultIsDataPlaneSafe("") {
		t.Fatal("unknown faults must not be treated as data-plane-safe")
	}
	for _, kind := range []string{"controlplane-kill", "database-restart", "agent-partition", "clock-skew"} {
		if !stressFaultIsDataPlaneSafe(kind) {
			t.Fatalf("%s should be data-plane safe", kind)
		}
	}
}

func TestReplicaRedirectIsNotServiceAgreement(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, delivery.LiveOwnerRedirectPrefix+"192.0.2.1:9443")
	if !replicaRPCRedirected(err) {
		t.Fatal("live-owner redirect must be classified as a replica redirect")
	}
	expected := stressService{Index: 0, ID: "service", Environment: "env", Marker: "owner", Revision: 2}
	owner := &platformv1.Service{Id: "service", EnvironmentId: "env", Spec: stressSpec("owner"), SpecRevision: 2}
	if err := checkStressService(expected, owner); err != nil {
		t.Fatal(err)
	}
	// A standby that followed the redirect would return the same owner body.
	// The raw-client probe must not treat that redirect as a second attestation.
	if replicaRPCRedirected(nil) {
		t.Fatal("success is not a redirect")
	}
}

func TestAmbiguousWriteAlternativeRetained(t *testing.T) {
	expected := stressService{Index: 0, ID: "service", Environment: "env", Marker: "acked", Revision: 2, Alternatives: []string{"late-commit"}}
	late := &platformv1.Service{Id: "service", EnvironmentId: "env", Spec: stressSpec("late-commit"), SpecRevision: 3}
	if err := checkStressService(expected, late); err != nil {
		t.Fatalf("late commit of an ambiguous write must still be accepted: %v", err)
	}
}

func TestStressAmbiguousClassification(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Aborted, codes.Unknown, codes.ResourceExhausted, codes.Internal} {
		if !stressAmbiguous(status.Error(code, "x")) {
			t.Fatalf("%v should be ambiguous", code)
		}
	}
	for _, code := range []codes.Code{codes.NotFound, codes.InvalidArgument, codes.PermissionDenied} {
		if stressAmbiguous(status.Error(code, "x")) {
			t.Fatalf("%v should not be ambiguous", code)
		}
	}
}

func TestStressOracleDetectsLostAcknowledgedWritesAndResolvesAmbiguity(t *testing.T) {
	expected := stressService{Index: 0, ID: "service", Environment: "env", Marker: "new", Revision: 2}
	actual := &platformv1.Service{Id: "service", EnvironmentId: "env", Spec: stressSpec("new"), SpecRevision: 2}
	if err := checkStressService(expected, actual); err != nil {
		t.Fatal(err)
	}
	actual.Spec = stressSpec("old")
	if err := checkStressService(expected, actual); err == nil {
		t.Fatal("lost acknowledged write passed")
	}
	expected.Marker = "old"
	expected.Alternatives = []string{"new", "other"}
	expected.Revision = 1
	if err := checkStressService(expected, actual); err != nil {
		t.Fatalf("ambiguous write must allow prior value: %v", err)
	}
	actual.Spec = stressSpec("other")
	if err := checkStressService(expected, actual); err != nil {
		t.Fatalf("ambiguous write must allow attempted value: %v", err)
	}
	actual.Spec = stressSpec("alien")
	if err := checkStressService(expected, actual); err == nil {
		t.Fatal("unattempted value passed")
	}
	actual.Spec = stressSpec("new")
	actual.SpecRevision = 0
	if err := checkStressService(expected, actual); err == nil {
		t.Fatal("revision regression passed")
	}
	actual.SpecRevision = 2
	actual.EnvironmentId = "other-tenant"
	if err := checkStressService(expected, actual); err == nil {
		t.Fatal("tenant identity changed")
	}
}

func TestStressStatsBreakingPoints(t *testing.T) {
	stats := &stressStats{Codes: make(map[string]int64)}
	for i := 0; i < 98; i++ {
		stats.add(10*time.Millisecond, nil)
	}
	stats.add(200*time.Millisecond, nil)
	stats.add(time.Second, status.Error(codes.Unavailable, "offline"))
	stats.finish(2 * time.Second)
	if stats.Requests != 100 || stats.P50MS != 10 || stats.P95MS != 10 || stats.P99MS != 200 || stats.RPS != 50 {
		t.Fatalf("incorrect statistics: requests=%d p50=%d p95=%d p99=%d RPS=%v", stats.Requests, stats.P50MS, stats.P95MS, stats.P99MS, stats.RPS)
	}
	opts := stressOptions{MaxP99: 250 * time.Millisecond, MaxErrorRate: .01}
	if err := stats.check(opts); err != nil {
		t.Fatal(err)
	}
	opts.MaxP99 = 100 * time.Millisecond
	if err := stats.check(opts); err == nil {
		t.Fatal("latency breaking point ignored")
	}
	opts.MaxP99 = time.Second
	opts.MaxErrorRate = 0
	if err := stats.check(opts); err == nil {
		t.Fatal("error-rate breaking point ignored")
	}
	stats.addWatchAnomaly(true, false)
	opts.MaxErrorRate = 1
	if err := stats.check(opts); err == nil {
		t.Fatal("watch index regression ignored")
	}
	stats.add(time.Duration(math.MaxInt64), nil) // saturates histogram instead of allocating per request
}

func TestStressStatsFoldAggregatesHistograms(t *testing.T) {
	stats := &stressStats{Codes: make(map[string]int64)}
	stats.fold(stressHTTPReport{Requests: 2, Errors: 1, Codes: map[string]int64{"wrong-content": 1}, Seconds: 1, Buckets: []int64{0, 2}})
	stats.fold(stressHTTPReport{Requests: 1, Codes: map[string]int64{"OK": 1}, Seconds: 2, Buckets: []int64{0, 0, 1}})
	stats.finish(2 * time.Second)
	if stats.Requests != 3 || stats.Errors != 1 || stats.Codes["wrong-content"] != 1 || stats.Codes["OK"] != 1 {
		t.Fatalf("fold lost samples: %+v", stats)
	}
	if stats.P50MS != 1 || stats.P99MS != 2 {
		t.Fatalf("folded percentiles wrong: p50=%d p99=%d", stats.P50MS, stats.P99MS)
	}
}

func TestChooseHTTPFlowsStaysInProjectAndPrefersAnotherAgent(t *testing.T) {
	services := []stressService{
		{Index: 0, ID: "s0", Environment: "a"},
		{Index: 1, ID: "s1", Environment: "a"},
		{Index: 2, ID: "s2", Environment: "b"},
		{Index: 3, ID: "s3", Environment: "b"},
	}
	allocations := map[string]*platformv1.AllocationStatus{
		"s0": {AllocationId: "alloc0", AgentId: "agent-a"},
		"s1": {AllocationId: "alloc1", AgentId: "agent-b"},
		"s2": {AllocationId: "alloc2", AgentId: "agent-a"},
		"s3": {AllocationId: "alloc3", AgentId: "agent-b"},
	}
	flows := chooseHTTPFlows(services, allocations, 2)
	if len(flows) != 2 {
		t.Fatalf("got %d flows", len(flows))
	}
	for _, flow := range flows {
		if flow.Source.Environment != flow.Target.Environment {
			t.Fatalf("cross-project flow: %+v", flow)
		}
		if flow.Source.Index == flow.Target.Index {
			t.Fatalf("flow used its own target as source: %+v", flow)
		}
	}
	if flows[0].Source.ID != "s1" || flows[0].Target.ID != "s0" {
		t.Fatalf("did not prefer a different-agent source: %+v", flows[0])
	}
	if len(chooseHTTPFlows(services, map[string]*platformv1.AllocationStatus{}, 2)) != 0 {
		t.Fatal("flows chosen without allocations")
	}
}

func TestChooseHTTPFlowsSupportsOneServiceEnvironment(t *testing.T) {
	services := []stressService{{Index: 0, ID: "s0", Environment: "a"}}
	allocations := map[string]*platformv1.AllocationStatus{"s0": {AllocationId: "alloc0", AgentId: "agent-a"}}
	flows := chooseHTTPFlows(services, allocations, 1)
	if len(flows) != 1 || flows[0].Source.ID != "s0" || flows[0].Target.ID != "s0" {
		t.Fatalf("single-service flow = %+v", flows)
	}
}

func TestReserveHTTPFlowsProtectsWorkloadServices(t *testing.T) {
	reserved := map[int]bool{2: true}
	reserveHTTPFlows([]httpFlow{{Source: stressService{Index: 0}, Target: stressService{Index: 1}}}, reserved)
	for _, index := range []int{0, 1, 2} {
		if !reserved[index] {
			t.Fatalf("service %d not reserved", index)
		}
	}
}

type fakeStressConn struct {
	grpc.ClientConnInterface
	calls int
	err   error
}

func (c *fakeStressConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	c.calls++
	return c.err
}

func TestStressClientFollowsOnlyExplicitKnownOwnerRedirects(t *testing.T) {
	primary := &fakeStressConn{err: status.Error(codes.FailedPrecondition, delivery.LiveOwnerRedirectMessage("owner:9444"))}
	owner := &fakeStressConn{}
	conn := stressConnection{primary, map[string]grpc.ClientConnInterface{"owner:9444": owner}}
	if err := conn.Invoke(context.Background(), "UpdateService", nil, nil); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 1 || owner.calls != 1 {
		t.Fatal("did not follow redirect")
	}
	primary.err = io.ErrUnexpectedEOF
	if err := conn.Invoke(context.Background(), "UpdateService", nil, nil); err != io.ErrUnexpectedEOF || owner.calls != 1 {
		t.Fatal("ambiguous mutation was replayed")
	}
	primary.err = status.Error(codes.FailedPrecondition, delivery.LiveOwnerRedirectMessage("outside:9444"))
	if err := conn.Invoke(context.Background(), "UpdateService", nil, nil); err == nil || owner.calls != 1 {
		t.Fatal("followed redirect outside fixture")
	}
}

func TestStressListDetectsMissingDuplicateAndForeignServices(t *testing.T) {
	own := &platformv1.Service{Id: "owned", EnvironmentId: "env"}
	foreign := &platformv1.Service{Id: "foreign", EnvironmentId: "other"}
	expected := map[string]bool{"owned": true}
	for _, services := range [][]*platformv1.Service{nil, {own, own}, {own, foreign}} {
		if err := checkStressList("env", expected, &platformv1.ListServicesResponse{Services: services}); err == nil {
			t.Fatalf("invalid list accepted: %v", services)
		}
	}
	if err := checkStressList("env", expected, &platformv1.ListServicesResponse{Services: []*platformv1.Service{own}}); err != nil {
		t.Fatal(err)
	}
}

func TestParseAndDiffAgentAllocations(t *testing.T) {
	state, err := parseAgentAllocations("netns=alloc-a,alloc-b\nctr=alloc-a,alloc-b\ndesired=alloc-a,alloc-b\n")
	if err != nil {
		t.Fatal(err)
	}
	if !state.NetNS["alloc-a"] || !state.Containers["alloc-b"] || !state.Desired["alloc-b"] {
		t.Fatalf("parse lost allocations: %+v", state)
	}
	if err := diffStressAgentState("agent-a", map[string]bool{"alloc-a": true, "alloc-b": true}, state); err != nil {
		t.Fatal(err)
	}
	if err := diffStressAgentState("agent-a", map[string]bool{"alloc-a": true}, state); err == nil {
		t.Fatal("leaked allocation accepted")
	}
	if err := diffStressAgentState("agent-a", map[string]bool{"alloc-a": true, "alloc-c": true}, state); err == nil {
		t.Fatal("missing allocation accepted")
	}
	if _, err := parseAgentAllocations("netns=alloc-a\nctr=alloc-a\n"); err == nil {
		t.Fatal("missing desired line accepted")
	}
}
