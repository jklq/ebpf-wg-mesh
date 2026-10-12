package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func fleetRollingPlan(t *testing.T) (Plan, State, Inventory) {
	t.Helper()
	i, r, inv := fixture(8)
	for _, role := range []Role{ControlPlane, Console, Builder, Envoy, Registry} {
		component := i.Components[role]
		component.Replicas = 8
		i.Components[role] = component
	}
	state := applied(build(t, i, r, State{}, inv, false))
	i.Release, r.ID = "r44", "r44"
	p := build(t, i, r, state, inv, false)
	requireComplete(t, p)
	return p, state, inv
}

func TestRolloutWavesRetainCapacityAndVerifyCanaryAndPreviousWave(t *testing.T) {
	p, _, _ := fleetRollingPlan(t)
	for _, role := range []Role{ControlPlane, Console, Builder, Envoy, Registry} {
		var waves [][][]Operation
		for i := 0; i < len(p.Operations); {
			chains := concurrentChains(p.Operations, i)
			if len(chains) == 0 {
				i++
				continue
			}
			if p.Operations[i].Phase == string(role) {
				waves = append(waves, chains)
			}
			for _, chain := range chains {
				i += len(chain)
			}
		}
		if len(waves) != 5 || len(waves[0]) != 1 {
			t.Fatalf("%s must verify one canary before bounded waves: %v", role, waves)
		}
		var previous []string
		for n, wave := range waves {
			if len(wave) > 2 {
				t.Fatalf("%s wave takes more than one quarter of capacity", role)
			}
			var terminals []string
			for _, chain := range wave {
				if n > 0 && !slices.Equal(chain[0].Requires, previous) {
					t.Fatalf("%s can advance before every previous replica verifies", role)
				}
				for index, op := range chain[1:] {
					if !slices.Equal(op.Requires, []string{chain[index].ID}) {
						t.Fatal("replica can bypass its own drain or configuration")
					}
				}
				terminal := chain[len(chain)-1]
				if role == Builder && terminal.Hook != "builder-resume" || role != Builder && terminal.Kind != "install" {
					t.Fatalf("wave ends before replica verification/admission: %+v", terminal)
				}
				terminals = append(terminals, terminal.ID)
			}
			slices.Sort(terminals)
			previous = terminals
		}
	}
	// Small roles, database quorums, initial install and restore remain serial.
	for _, tc := range []struct {
		role    Role
		n, want int
	}{{Console, 1, 1}, {Console, 3, 1}, {Console, 7, 1}, {Console, 8, 2}, {Console, 1000, 16}, {Agent, 19, 1}, {Agent, 20, 2}, {Agent, 1000, 16}} {
		if got := rolloutWave(tc.role, tc.n); got != tc.want {
			t.Fatalf("%s with %d replicas: %d restarts, want %d", tc.role, tc.n, got, tc.want)
		}
	}
	for _, op := range p.Operations {
		if op.Phase == "database-runtime" && len(p.waveMates(*op.Placement)) != 0 {
			t.Fatal("database members restart concurrently")
		}
	}
	groups := map[string][]Operation{}
	for _, op := range p.Operations {
		if op.Phase == "console" {
			groups[op.Phase] = append(groups[op.Phase], op)
		}
	}
	for _, mode := range []workflowMode{freshMode, restoreMode} {
		for _, wave := range rolloutWaves(mode, "console", groups["console"]) {
			if len(wave) != 1 {
				t.Fatal("fresh installation or restore used restart waves")
			}
		}
	}
}

// waveDriver serializes the fake's maps but releases its lock while installs
// wait for their wave mate. A serial engine cannot pass this barrier.
type waveDriver struct {
	*fakeDriver
	mu       sync.Mutex
	barrier  map[string]chan struct{}
	arrived  map[string]int
	failedID string
}

func (d *waveDriver) Observe(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fakeDriver.Observe(ctx, p, state, op)
}

func (d *waveDriver) Execute(ctx context.Context, p Plan, state State, op Operation) (Binding, error) {
	if op.Kind == "install" && op.Placement != nil && len(p.waveMates(*op.Placement)) > 0 {
		key := fmt.Sprint(op.Phase, op.Requires)
		// Install requirements differ within a wave, so use its head requirements.
		for _, head := range p.Operations {
			if head.Phase == op.Phase && head.Placement != nil && head.Placement.Instance == op.Placement.Instance {
				key = fmt.Sprint(op.Phase, head.Requires)
				break
			}
		}
		d.mu.Lock()
		if d.barrier[key] == nil {
			d.barrier[key] = make(chan struct{})
		}
		ready := d.barrier[key]
		d.arrived[key]++
		if d.arrived[key] == len(p.waveMates(*op.Placement))+1 {
			close(ready)
		}
		d.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return Binding{}, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if op.ID == d.failedID {
		// The mate must still finish its chain, especially builder-resume.
		d.count[op.ID]++
		return Binding{}, errors.New("one wave replica failed")
	}
	return d.fakeDriver.Execute(ctx, p, state, op)
}

func TestWaveFailureFinishesHealthyChainsBlocksLaterWavesAndResumes(t *testing.T) {
	p, state, inv := fleetRollingPlan(t)
	var firstWave [][]Operation
	for n, op := range p.Operations {
		if op.Phase == "builder" {
			if wave := concurrentChains(p.Operations, n); len(wave) > 1 {
				firstWave = wave
				break
			}
		}
	}
	if len(firstWave) != 2 {
		t.Fatal("fixture has no concurrent builder wave")
	}
	var failed string
	for _, op := range firstWave[0] {
		if op.Kind == "install" {
			failed = op.ID
		}
	}
	store, _, _ := testStore(t)
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	d := &waveDriver{fakeDriver: fake(inv), barrier: map[string]chan struct{}{}, arrived: map[string]int{}, failedID: failed}
	e := Engine{Store: store, Driver: d}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.Apply(ctx, p); err == nil || ctx.Err() != nil {
		t.Fatal("wave did not execute concurrently and fail at its replica", err)
	}
	survivor := firstWave[1][len(firstWave[1])-1]
	if d.count[survivor.ID] != 1 {
		t.Fatal("healthy wave mate was left drained")
	}
	partial, err := store.Read()
	if err != nil || partial.Progress == nil || partial.Bundle.ID != state.Bundle.ID {
		t.Fatal("failure lost resumable previous deployment", err)
	}
	if _, done := partial.Progress.Completed[survivor.ID]; !done {
		t.Fatal("healthy chain verification was not persisted")
	}
	waveEnd := slices.IndexFunc(p.Operations, func(op Operation) bool { return op.ID == survivor.ID })
	for _, op := range p.Operations[waveEnd+1:] {
		if d.count[op.ID] != 0 {
			t.Fatalf("later wave or phase started after failure: %+v", op)
		}
	}
	d.failedID = ""
	if err := e.Apply(ctx, p); err != nil {
		t.Fatal("wave did not resume", err)
	}
	if d.count[survivor.ID] != 1 || d.count[failed] != 2 {
		t.Fatal("resume repeated a verified replica or skipped the failed one")
	}
	final, err := store.Read()
	if err != nil || final.Progress != nil || final.Bundle.ID != p.Release.ID {
		t.Fatal("completed waves did not commit", err)
	}
}

func TestWavePeerGateSkipsOnlyConcurrentReplicas(t *testing.T) {
	p, _, _ := fleetRollingPlan(t)
	var target Placement
	for _, pl := range p.Placements {
		if pl.Role == Console && len(p.waveMates(pl)) == 1 {
			target = pl
			break
		}
	}
	remote := &replicaRemote{}
	d := SSHDriver{Remote: remote, Adapter: func(Installation, Host) (Adapter, error) { return imported{}, nil }}
	if err := d.verifyReplicaPeers(context.Background(), p, State{}, target); err != nil {
		t.Fatal(err)
	}
	if len(remote.checked) != 6 {
		t.Fatalf("peer gate checked %d hosts, want six surviving replicas", len(remote.checked))
	}
	for _, mate := range p.Placements {
		if p.waveMates(target)[mate.Instance] && slices.Contains(remote.checked, mate.Host) {
			t.Fatal("concurrent restart blocked its wave mate")
		}
	}
	remote.unready = remote.checked[0]
	if err := d.verifyReplicaPeers(context.Background(), p, State{}, target); err == nil {
		t.Fatal("unready surviving peer did not block restart")
	}
}
