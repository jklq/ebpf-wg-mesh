package deploy

import (
	"context"
	"slices"
	"strings"
	"testing"

	"ebof-wg-mesh/internal/continuity"
)

func nextRelease(r Release, id string) Release {
	next := snapshot(r)
	next.ID = id
	for role, program := range next.Programs {
		a := program.Artifacts["amd64"]
		a.SHA256 = Digest([]string{id, string(role)})
		program.Artifacts["amd64"] = a
		next.Programs[role] = program
	}
	return next
}

func listenerFixture(t *testing.T) (Installation, Release, State, Inventory) {
	t.Helper()
	i, r, inv := fixture(1)
	r.Tools = map[string]map[string]Artifact{"continuity": {"amd64": {URL: "https://release.example/continuity", SHA256: strings.Repeat("3", 64), Bytes: 1 << 20}}}
	ports := map[Role]string{ControlPlane: "9443", Console: "3000", Registry: "5000", Envoy: "443"}
	for role, port := range ports {
		program := r.Programs[role]
		program.Listeners = []continuity.Listener{{Address: ":" + port, Env: "LISTEN"}}
		r.Programs[role] = program
	}
	state := applied(build(t, i, r, State{}, inv, false))
	next := nextRelease(r, "r43")
	next.Programs[Database] = r.Programs[Database]
	i.Release = next.ID
	return i, next, state, inv
}

func TestUpgradeHandsOverListenersAndKeepsAnUnchangedDatabase(t *testing.T) {
	i, r, state, inv := listenerFixture(t)
	p := build(t, i, r, state, inv, false)
	requireComplete(t, p)
	actions := map[Role][]string{}
	for _, op := range p.Operations {
		if op.Placement == nil || op.Phase == "stage" {
			continue
		}
		if op.Placement.Role == Database {
			t.Fatalf("unchanged database node restarts: %+v", op)
		}
		kind := op.Kind
		if kind == "hook" {
			kind = op.Hook
		}
		actions[op.Placement.Role] = append(actions[op.Placement.Role], kind)
	}
	for _, role := range []Role{ControlPlane, Console, Registry, Envoy} {
		if !slices.Equal(actions[role], []string{"configure", "install", "finish"}) {
			t.Fatalf("%s does not hand over its listeners: %v", role, actions[role])
		}
	}
	if !slices.Equal(actions[Builder], []string{"builder-drain", "configure", "install", "builder-resume"}) {
		t.Fatalf("builder does not drain around its restart: %v", actions[Builder])
	}
	if peak := p.Upgrade.Peak["a"]; peak.MemoryMiB != p.Reservations["a"].MemoryMiB+i.Components[ControlPlane].Resources.MemoryMiB {
		t.Fatalf("overlap budget %+v for reservations %+v", peak, p.Reservations["a"])
	}
	if len(p.Upgrade.Limitations) != 2 {
		t.Fatalf("agent and builder limitations hidden: %v", p.Upgrade.Limitations)
	}

	r.Programs[Database] = nextRelease(r, "r44").Programs[Database]
	if _, err := BuildPlan(i, r, state, inv, false, testNow); err == nil || !strings.Contains(err.Error(), "three database members") {
		t.Fatalf("a single-node database restart was admitted: %v", err)
	}
}

func TestUpgradeRequiresHeadroomForOneOverlappingProcess(t *testing.T) {
	i, r, state, inv := listenerFixture(t)
	h := i.Hosts[0]
	h.Capacity.MemoryMiB = h.Reserve.MemoryMiB
	for role := range i.Components {
		h.Capacity.MemoryMiB += ComponentResources(i, r, role).MemoryMiB
	}
	i.Hosts[0] = h
	observed := inv.Hosts[h.ID]
	observed.Capacity = h.Capacity
	inv.Hosts[h.ID] = observed
	p := build(t, i, r, state, inv, false)
	if !slices.ContainsFunc(p.Unmet, func(u Requirement) bool { return u.Code == "upgrade-capacity" }) {
		t.Fatalf("upgrade without overlap headroom admitted: %+v", p.Unmet)
	}
}

func TestNewListenerOwnerRestartsSoleReplicasVisibly(t *testing.T) {
	i, r, state, inv := listenerFixture(t)
	a := r.Tools["continuity"]["amd64"]
	a.SHA256 = strings.Repeat("9", 64)
	r.Tools["continuity"]["amd64"] = a
	p := build(t, i, r, state, inv, false)
	requireComplete(t, p)
	restarts := 0
	for _, limitation := range p.Upgrade.Limitations {
		if strings.Contains(limitation, "restarts its listener owner") {
			restarts++
		}
	}
	if restarts != 4 {
		t.Fatalf("sole replicas restarting their owners are not reported: %v", p.Upgrade.Limitations)
	}
}

func TestInterruptedUpgradeCanRollBackOrRepair(t *testing.T) {
	for _, action := range []string{"rollback", "repair"} {
		t.Run(action, func(t *testing.T) {
			i, r, state, inv := listenerFixture(t)
			p := build(t, i, r, state, inv, false)
			requireComplete(t, p)
			store, _, _ := testStore(t)
			if err := store.Write(state); err != nil {
				t.Fatal(err)
			}
			driver := fake(inv)
			driver.failHook = "production-verify"
			engine := Engine{Store: store, Driver: driver}
			if err := engine.Apply(context.Background(), p); err == nil {
				t.Fatal("failure injection did not interrupt the upgrade")
			}
			driver.failHook = ""
			wanted := state.Bundle.ID
			if action == "rollback" {
				if err := engine.Rollback(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				fixed := nextRelease(r, "r44")
				fixed.Programs[Database] = r.Programs[Database]
				wanted = fixed.ID
				if err := engine.Repair(context.Background(), fixed); err != nil {
					t.Fatal(err)
				}
			}
			after, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if after.Progress != nil || after.Bundle.ID != wanted {
				t.Fatalf("recovery did not commit %s: %+v", wanted, after.Bundle.ID)
			}
		})
	}
}
