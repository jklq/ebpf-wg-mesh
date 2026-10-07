package recovery

import (
	"slices"
	"testing"
	"time"
)

func TestSurvivingFleetReportPreservesNewerUnknownAndUnreachableResources(t *testing.T) {
	cutoff := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	started := cutoff.Add(time.Hour)
	matching := FleetAllocation{ID: "matching", ServiceID: "service", EnvironmentID: "environment", DeploymentID: "deployment", SpecRevision: 1, RolloutGeneration: 1, IPv4: "10.0.0.2", CreatedAt: cutoff.Add(-time.Hour)}
	newer := FleetAllocation{ID: "newer", ServiceID: "new-service", EnvironmentID: "new-environment", SpecRevision: 9, IPv4: "10.0.0.3", CreatedAt: cutoff.Add(time.Minute)}
	input := FleetInput{CapturedAt: started, Desired: []FleetHost{{ID: "survivor", LocalStoreID: "store", Allocations: []FleetAllocation{matching}}, {ID: "unreachable", Reservations: []NetworkReservation{{Prefix: "10.1.0.0/24"}}}}, Observed: []FleetHost{
		{ID: "survivor", LocalStoreID: "store", Generation: "recovery", Reachable: true, AuthorityResolved: true, Allocations: []FleetAllocation{matching, newer}, Reservations: []NetworkReservation{{Prefix: "10.0.0.0/24"}}},
		{ID: "external-only", Generation: "recovery", Reachable: true, Isolated: true, AuthorityResolved: true, Reservations: []NetworkReservation{{Prefix: "10.2.0.0/24", EnvironmentID: "new-environment", Identity: 80}}},
		{ID: "unreachable", Isolated: true, Reservations: []NetworkReservation{{Prefix: "10.1.0.0/24"}}},
	}, Resources: []FleetResource{{Kind: "release", ID: "after-backup", CreatedAt: cutoff.Add(time.Minute)}}}
	report, err := CompareFleet(input, "installation", "recovery", "selected-release", cutoff, started, started.Add(time.Minute), true)
	if err != nil || report.Blocked {
		t.Fatal("isolated fleet was not reconcilable", report, err)
	}
	has := func(kind, id string) bool {
		return slices.ContainsFunc(report.Differences, func(d FleetDifference) bool { return d.Kind == kind && (d.Resource == id || d.Host == id) })
	}
	for _, want := range []struct{ kind, id string }{{"adopt-allocation", "matching"}, {"quarantined-allocation", "newer"}, {"after-cutoff", "newer"}, {"after-cutoff", "release/after-backup"}, {"unknown-host", "external-only"}, {"unreachable-host", "unreachable"}} {
		if !has(want.kind, want.id) {
			t.Fatalf("missing difference %s %s: %+v", want.kind, want.id, report)
		}
	}
	if !slices.ContainsFunc(report.Reservations, func(r NetworkReservation) bool { return r.Owner == "unreachable" && r.Prefix == "10.1.0.0/24" }) {
		t.Fatal("unreachable host range was released")
	}
	digest := report.ApprovalDigest()
	report.ElapsedSeconds += 120
	if digest != report.ApprovalDigest() {
		t.Fatal("elapsed time invalidated report approval")
	}
	input.Observed[0].Allocations[0].SpecRevision = 2
	revised, err := CompareFleet(input, "installation", "recovery", "selected-release", cutoff, started, started.Add(time.Minute), true)
	if err != nil || revised.ApprovalDigest() == digest || !slices.ContainsFunc(revised.Differences, func(d FleetDifference) bool { return d.Kind == "changed-allocation" && d.Resource == "matching" }) {
		t.Fatal("newer revision did not invalidate adoption and report approval", err)
	}
}

func TestRecoveryReportBlocksNetworkConflictAndStaleInventory(t *testing.T) {
	now := time.Now().UTC()
	input := FleetInput{CapturedAt: now, Observed: []FleetHost{{ID: "a", Isolated: true, Reservations: []NetworkReservation{{Prefix: "10.0.0.0/24", Identity: 1, EnvironmentID: "one"}}}, {ID: "b", Isolated: true, Reservations: []NetworkReservation{{Prefix: "10.0.0.2/32", Identity: 1, EnvironmentID: "two"}}}}}
	r, err := CompareFleet(input, "installation", "generation", "release", now.Add(-time.Hour), now, now, true)
	if err != nil || !r.Blocked || !slices.ContainsFunc(r.Differences, func(d FleetDifference) bool { return d.Kind == "network-conflict" }) {
		t.Fatal("overlapping ranges and reused environment identity were accepted", r, err)
	}
	input.CapturedAt = now.Add(-time.Second)
	if _, err := CompareFleet(input, "installation", "generation", "release", now.Add(-time.Hour), now, now, true); err == nil {
		t.Fatal("older external inventory accepted")
	}
}

func TestReportIdentifiesConflictingAddressesOnOneHostAndUnallocatedEnvironment(t *testing.T) {
	now := time.Now().UTC()
	input := FleetInput{CapturedAt: now, DesiredNetworks: []NetworkReservation{{Owner: "environment/old", EnvironmentID: "old", Identity: 7}}, Observed: []FleetHost{{ID: "host", Generation: "generation", Reachable: true, AuthorityResolved: true, Reservations: []NetworkReservation{{EnvironmentID: "new", Identity: 7}}, Allocations: []FleetAllocation{{ID: "one", IPv4: "10.0.0.2"}, {ID: "two", IPv4: "10.0.0.2"}}}}}
	report, err := CompareFleet(input, "installation", "generation", "release", now.Add(-time.Hour), now, now, true)
	if err != nil || !report.Blocked {
		t.Fatal("conflicts were accepted", report, err)
	}
	for _, resource := range []string{"10.0.0.2", "network-identity/7"} {
		if !slices.ContainsFunc(report.Differences, func(d FleetDifference) bool { return d.Kind == "network-conflict" && d.Resource == resource }) {
			t.Fatal("report omitted concrete conflict", resource, report)
		}
	}
}
