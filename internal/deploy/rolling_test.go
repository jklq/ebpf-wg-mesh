package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func rollingPlan(t *testing.T, schemaChange bool) (Plan, State, Inventory) {
	t.Helper()
	i, r, inv := fixture(3)
	state := applied(build(t, i, r, State{}, inv, false))
	r.ID, i.Release = "r44", "r44"
	if schemaChange {
		r.Schema++
		r.SchemaChanges = map[string]SchemaChange{state.Bundle.ID: {
			Expand:   Hook{Command: []string{"/opt/expand"}, Verify: []string{"/opt/check-expand"}},
			Backfill: Hook{Command: []string{"/opt/backfill"}, Verify: []string{"/opt/check-backfill"}},
		}}
	}
	p := build(t, i, r, state, inv, false)
	requireComplete(t, p)
	return p, state, inv
}

func TestRollingUpgradeOrdersReplicasAndOnlineSchemaStages(t *testing.T) {
	p, _, _ := rollingPlan(t, true)
	position := map[string]int{}
	for n, op := range p.Operations {
		position[op.Phase] = n
		if n > 0 && (len(op.Requires) != 1 || op.Requires[0] != p.Operations[n-1].ID) {
			t.Fatalf("replica can bypass the previous verification: %+v", op)
		}
	}
	for _, pair := range [][2]string{{"upgrade-prepare", "pre-backup"}, {"pre-backup", "database-runtime"}, {"database-runtime", "database-finalize"}, {"database-finalize", "schema-expand"}, {"schema-expand", "schema-overlap"}, {"schema-overlap", "controlplane"}, {"envoy", "schema-backfill"}, {"schema-backfill", "schema-final"}, {"schema-final", "upgrade-schedule"}} {
		if position[pair[0]] >= position[pair[1]] {
			t.Fatalf("unsafe ordering %s -> %s", pair[0], pair[1])
		}
	}
	for _, pl := range p.Placements {
		if pl.Role != Database {
			continue
		}
		if !slices.ContainsFunc(p.Operations, func(op Operation) bool {
			return op.Kind == "database-upgrade" && op.Placement != nil && *op.Placement == pl
		}) {
			t.Fatalf("node %s has no rolling gate", pl.Instance)
		}
	}
	for _, mutate := range []func(*Plan){
		func(p *Plan) {
			p.Operations = slices.DeleteFunc(p.Operations, func(op Operation) bool { return op.Kind == "schema-backfill" })
		},
		func(p *Plan) {
			for n := range p.Operations {
				if p.Operations[n].Kind == "database-upgrade" {
					p.Operations[n].Kind = "install"
					p.Operations[n].Hook = ""
					return
				}
			}
		},
	} {
		changed := snapshot(p)
		mutate(&changed)
		changed.ID = changed.digest()
		if err := changed.Validate(); err == nil {
			t.Fatal("altered plan bypassed rolling safety")
		}
	}
}

type pendingReplicaDriver struct {
	*fakeDriver
	pending string
}

func (d *pendingReplicaDriver) Observe(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	if op.ID == d.pending {
		return false, Evidence{}, nil
	}
	return d.fakeDriver.Observe(ctx, p, state, op)
}

func TestRollingFailureKeepsOtherReplicasServingAndResumesTheSameReplica(t *testing.T) {
	p, state, inv := rollingPlan(t, false)
	var installs []Operation
	for _, op := range p.Operations {
		if op.Kind == "database-upgrade" {
			installs = append(installs, op)
		}
	}
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	d := &pendingReplicaDriver{fakeDriver: fake(inv), pending: installs[1].ID}
	e := Engine{Store: store, Driver: d}
	if err := e.Apply(context.Background(), p); err == nil {
		t.Fatal("unready replica did not block the rollout")
	}
	if d.count[installs[0].ID] != 1 || d.count[installs[1].ID] != 1 || d.count[installs[2].ID] != 0 || d.paused {
		t.Fatal("pending replica interrupted surviving nodes or global automation")
	}
	partial, err := store.Read()
	if err != nil || partial.Progress == nil || partial.Bundle.ID != state.Bundle.ID || partial.LastBackup.Backup == "" {
		t.Fatal("interrupted rollout lost its prior deployment and backup", err)
	}
	d.pending = ""
	// A long rollout may outlive the freshness window of its prior recovery
	// point. It must retain that point instead of capturing mixed versions.
	d.failObserveHook = "backup"
	if err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, op := range installs {
		if d.count[op.ID] != 1 {
			t.Fatal("retry restarted an already installed replica")
		}
	}
	for _, op := range p.Operations {
		if op.Phase == "pre-backup" && d.count[op.ID] != 1 {
			t.Fatal("retry replaced the pre-upgrade recovery point")
		}
	}
	final, err := store.Read()
	if err != nil || final.Progress != nil || final.Bundle.ID != p.Release.ID {
		t.Fatal("rollout did not commit", err)
	}
}

func TestSchemaCleanupRequiresALaterCompatibleRelease(t *testing.T) {
	p, state, inv := rollingPlan(t, true)
	for _, modify := range []func(*Release, *State){
		func(r *Release, _ *State) {
			c := r.SchemaChanges[state.Bundle.ID]
			c.Backfill = Hook{}
			r.SchemaChanges[state.Bundle.ID] = c
		},
		func(_ *Release, s *State) { old := *s.Bundle; old.SchemaCompatibility = false; s.Bundle = &old },
		func(r *Release, _ *State) {
			c := r.SchemaChanges[state.Bundle.ID]
			c.Contract = c.Expand
			r.SchemaChanges[state.Bundle.ID] = c
		},
	} {
		r, s := snapshot(p.Release), snapshot(state)
		modify(&r, &s)
		if _, err := BuildPlan(p.Installation, r, s, inv, false, testNow); err == nil {
			t.Fatal("unsafe schema transition accepted")
		}
	}
	previous := applied(p)
	r := snapshot(p.Release)
	r.ID = "r45"
	r.MinSchema = r.Schema
	r.Schema++
	r.SchemaChanges = map[string]SchemaChange{previous.Bundle.ID: {Contract: Hook{Command: []string{"/opt/contract"}, Verify: []string{"/opt/check-contract"}}}}
	i := p.Installation
	i.Release = r.ID
	cleanup := build(t, i, r, previous, inv, false)
	requireComplete(t, cleanup)
	lastRuntime, contract := -1, -1
	for n, op := range cleanup.Operations {
		if op.Kind == "install" {
			lastRuntime = n
		}
		if op.Kind == "schema-contract" {
			contract = n
		}
	}
	if contract <= lastRuntime {
		t.Fatal("cleanup precedes compatible code")
	}
}

type interruptedCleanupDriver struct {
	*fakeDriver
	contracted bool
	interrupt  bool
}

func (d *interruptedCleanupDriver) Execute(ctx context.Context, p Plan, state State, op Operation) (Binding, error) {
	binding, err := d.fakeDriver.Execute(ctx, p, state, op)
	if op.Kind == "schema-contract" && err == nil {
		d.contracted = true
		if d.interrupt {
			d.interrupt = false
			return binding, errors.New("interrupted after SQL cleanup")
		}
	}
	return binding, err
}
func (d *interruptedCleanupDriver) Observe(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	if op.Phase == "schema-overlap" && d.contracted {
		return false, Evidence{}, errors.New("old schema boundary no longer exists")
	}
	return d.fakeDriver.Observe(ctx, p, state, op)
}

func TestCleanupInterruptionResumesAfterTheOverlapBoundaryIsSuperseded(t *testing.T) {
	p, _, inv := rollingPlan(t, true)
	state := applied(p)
	r := snapshot(p.Release)
	r.ID = "r45"
	r.MinSchema = r.Schema
	r.Schema++
	r.SchemaChanges = map[string]SchemaChange{state.Bundle.ID: {Contract: Hook{Command: []string{"/opt/contract"}, Verify: []string{"/opt/check-contract"}}}}
	i := p.Installation
	i.Release = r.ID
	cleanup := build(t, i, r, state, inv, false)
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	d := &interruptedCleanupDriver{fakeDriver: fake(inv), interrupt: true}
	e := Engine{Store: store, Driver: d}
	if err := e.Apply(context.Background(), cleanup); err == nil || !d.contracted {
		t.Fatal("cleanup interruption was not retained")
	}
	if err := e.Apply(context.Background(), cleanup); err != nil {
		t.Fatal("superseded overlap gate blocked recovery of completed SQL cleanup", err)
	}
}

func TestRollingPlanRejectsAnUnhealthyClusterBeforeItStarts(t *testing.T) {
	p, state, inv := rollingPlan(t, false)
	inv.Database.Live = inv.Database.Live[:2]
	if _, err := BuildPlan(p.Installation, p.Release, state, inv, false, testNow); err == nil {
		t.Fatal("plan admitted another restart with a failed database member")
	}
}

type builderRetryDriver struct {
	*fakeDriver
	install, resume string
}

func (d *builderRetryDriver) Execute(ctx context.Context, p Plan, state State, op Operation) (Binding, error) {
	if op.ID == d.install {
		// The native driver drains immediately before every builder install.
		delete(d.done, d.resume)
	}
	return d.fakeDriver.Execute(ctx, p, state, op)
}

func TestRollingBuilderInstallRetryReopensItsResume(t *testing.T) {
	p, state, inv := rollingPlan(t, false)
	var install, resume, later string
	for _, op := range p.Operations {
		if op.Kind == "install" && op.Placement.Role == Builder && install == "" {
			install = op.ID
		}
		if op.Hook == "builder-resume" && resume == "" {
			resume = op.ID
		}
		if op.Kind == "install" && op.Placement.Role == Agent && later == "" {
			later = op.ID
		}
	}
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	d := &builderRetryDriver{fakeDriver: fake(inv), install: install, resume: resume}
	d.fail = later
	e := Engine{Store: store, Driver: d}
	if err := e.Apply(context.Background(), p); err == nil || d.count[resume] != 1 {
		t.Fatal("rollout did not interrupt after the builder resumed", err)
	}
	d.fail = ""
	delete(d.done, install) // That builder now requires another install.
	if err := e.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if d.count[install] != 2 || d.count[resume] != 2 {
		t.Fatal("retried builder was left drained after its verified install")
	}
}

type replicaRemote struct {
	checked []string
	scripts []string
	unready string
}

func (r *replicaRemote) Run(_ context.Context, _ Installation, host Host, script string) ([]byte, error) {
	r.checked = append(r.checked, host.ID)
	r.scripts = append(r.scripts, script)
	if host.ID == r.unready {
		return nil, errors.New("peer readiness failed")
	}
	return nil, nil
}

func (*replicaRemote) Upload(context.Context, Installation, Host, string, string, string) error {
	return errors.New("peer readiness must not upload files")
}

func TestRollingPeerGateChecksUpdatedReplicasAndExcludesRetiredOnRetry(t *testing.T) {
	i, release, _ := fixture(3)
	prior := snapshot(release)
	program := prior.Programs[ControlPlane]
	program.Ready = []string{"/opt/old-ready"}
	prior.Programs[ControlPlane] = program
	program.Ready = []string{"/opt/new-ready"}
	release.Programs[ControlPlane] = program
	target := Placement{Role: ControlPlane, Host: "a", Instance: "controlplane-a"}
	old := Placement{Role: ControlPlane, Host: "b", Instance: "controlplane-b"}
	peer := Placement{Role: ControlPlane, Host: "c", Instance: "controlplane-c"}
	p := Plan{Installation: i, Release: release, Previous: &AppliedDeployment{Release: prior, Placements: []Placement{target, old, peer}}, Placements: []Placement{target, peer}, Operations: []Operation{{ID: "retired", Kind: "stop", Placement: &old}, {ID: "joined", Kind: "install", Placement: &peer}}}
	state := State{Progress: &Progress{Completed: map[string]Evidence{"retired": {}, "joined": {}}}}
	remote := &replicaRemote{}
	driver := SSHDriver{Remote: remote, Adapter: func(Installation, Host) (Adapter, error) { return imported{}, nil }}
	if err := driver.verifyReplicaPeers(context.Background(), p, state, target); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(remote.checked, []string{"c"}) || !strings.Contains(remote.scripts[0], "new-ready") {
		t.Fatal("retired replica blocks a retry or updated peer uses the old verifier", remote.checked)
	}
	remote.unready = "c"
	if err := driver.verifyReplicaPeers(context.Background(), p, state, target); err == nil {
		t.Fatal("durable install receipt hid a failed surviving peer")
	}
}
