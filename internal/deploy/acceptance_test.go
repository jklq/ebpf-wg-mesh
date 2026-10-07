package deploy

import (
	"path/filepath"
	"testing"
)

func TestReferenceManifestsMatchTypedReleaseContract(t *testing.T) {
	dir := "../../infra/production/examples"
	r, err := Load[Release](filepath.Join(dir, "releases/r42.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"single-host", "vm-home", "providers-redundant", "regions"} {
		t.Run(name, func(t *testing.T) {
			i, err := Load[Installation](filepath.Join(dir, name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := i.Validate(r); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestExplicitRemovalDecommissionsBeforeInfrastructureDeletion(t *testing.T) {
	i, r, inv := fixture(4)
	p := build(t, i, r, State{}, inv, false)
	state := applied(p)
	i.Hosts[3].Binding.Provider = "hetzner"
	i.Hosts[3].FailureDomain = "hetzner/d"
	i.ManagementHost = "d"
	i.RetireHosts = []string{"a"}
	agents := i.Components[Agent]
	agents.Replicas = 3
	i.Components[Agent] = agents
	p = build(t, i, r, state, inv, false)
	requireComplete(t, p)
	decommission, verified, retired, deleted := -1, -1, -1, -1
	for n, op := range p.Operations {
		if op.Kind == "database-remove" && op.Host == "a" {
			decommission = n
		}
		if op.Hook == "database-verify" && n > decommission {
			verified = n
		}
		if op.Kind == "retire" && op.Host == "a" {
			retired = n
		}
		if op.Kind == "delete" && op.Host == "a" {
			deleted = n
		}
	}
	if !(decommission >= 0 && verified > decommission && retired > decommission && deleted > verified && deleted > retired) {
		t.Fatalf("unsafe deletion order: decommission=%d verified=%d retired=%d deleted=%d", decommission, verified, retired, deleted)
	}
	for _, pl := range p.Placements {
		if pl.Host == "a" {
			t.Fatal("placement retained on explicitly retired host")
		}
	}
}
func TestReconciliationSkipsUnavailableWorkersAndUsesSurvivingAdministration(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	state := applied(p)
	status := inv.Hosts["a"]
	status.Online = false
	inv.Hosts["a"] = status
	p = build(t, i, r, state, inv, true)
	requireComplete(t, p)
	if p.AdministrationHost == "a" {
		t.Fatal("controller depended on unavailable preferred management host")
	}
	for _, op := range p.Operations {
		if op.Host == "a" && (op.Kind == "stage" || op.Kind == "install") {
			t.Fatal("stateless reconciliation blocked on unavailable worker", op)
		}
	}
}
func TestUnverifiedStorageNeverEstablishesAvailability(t *testing.T) {
	i, r, inv := fixture(3)
	inv.Storage = nil
	p := build(t, i, r, State{}, inv, false)
	if p.Availability.OneHostFailure {
		t.Fatal("declared replication counted as verified durable storage")
	}
}

func TestChangedDatabasePlacementRequiresFreshReplicationVerification(t *testing.T) {
	i, r, inv := fixture(4)
	database := i.Components[Database]
	database.Hosts = []string{"b", "c", "d"}
	i.Components[Database] = database
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	if p.Availability.DatabaseRedundant || p.Availability.OneHostFailure {
		t.Fatal("old cluster replication established availability for unapplied members")
	}
}

func TestAvailabilityUsesObservedSurvivingCapacity(t *testing.T) {
	i, r, inv := fixture(3)
	i.Workload.CPUMillis = 6000
	for id, status := range inv.Hosts {
		status.Capacity.CPUMillis = 3000
		inv.Hosts[id] = status
	}
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	if p.Availability.OneHostFailure {
		t.Fatal("declared capacity exceeded the observed surviving capacity")
	}
}

func TestAppliedHostIdentityCannotBeReboundToAnotherMachine(t *testing.T) {
	i, r, inv := fixture(3)
	state := applied(build(t, i, r, State{}, inv, false))
	i.Hosts[0].Binding.ServerID = "another-machine"
	if _, err := BuildPlan(i, r, state, inv, false, testNow); err == nil {
		t.Fatal("new physical machine reused an applied identity")
	}
}

func TestBootstrapIssuesCredentialsBeforeConfiguringPlatformComponents(t *testing.T) {
	i, r, inv := fixture(1)
	p := build(t, i, r, State{}, inv, false)
	bootstrap, credentials := -1, -1
	for n, op := range p.Operations {
		if op.Hook == "platform-bootstrap" {
			bootstrap = n
		}
		if op.Hook == "credentials" {
			credentials = n
		}
		if op.Kind == "configure" && op.Placement.Role != Database {
			if !(bootstrap >= 0 && credentials > bootstrap && n > credentials) {
				t.Fatal("component credentials required before platform key bootstrap", op)
			}
		}
	}
}
