package deploy

import (
	"context"
	"fmt"
	"time"
)

// Repair replaces an interrupted release with a new immutable release while
// preserving its topology, authority, schema changes and rollback floor.
// Native observations decide which transitions remain; journaled attempts are
// not mistaken for successful routing or ownership changes.
func (e Engine) Repair(ctx context.Context, r Release) error {
	state, err := e.Store.Read()
	if err != nil {
		return err
	}
	if state.Progress == nil || state.Progress.Plan == nil {
		return fmt.Errorf("no interrupted upgrade to repair")
	}
	failed := *state.Progress.Plan
	if !failed.RollingUpgrade() || failed.Rollback {
		return fmt.Errorf("forward repair requires an interrupted rolling upgrade")
	}
	if r.ID == failed.Release.ID || r.Schema != failed.Release.Schema || r.ConsoleSchema != failed.Release.ConsoleSchema || r.Protocol != failed.Release.Protocol || r.Configuration != failed.Release.Configuration || Digest(r.SchemaChanges) != Digest(failed.Release.SchemaChanges) {
		return fmt.Errorf("repair requires a new release identity with the same configuration, protocol, schemas and schema changes")
	}
	i := snapshot(failed.Installation)
	i.Release = r.ID
	inv, err := e.Driver.Inventory(ctx, i, r, state)
	if err != nil {
		return err
	}
	p, err := BuildPlan(i, r, state, inv, false, time.Now())
	if err != nil {
		return err
	}
	if len(p.Unmet) > 0 {
		return fmt.Errorf("forward repair cannot safely transition: %v", p.Unmet)
	}
	p.SupersedesPlan = failed.ID
	p.ID = p.digest()
	if err = p.Validate(); err != nil {
		return err
	}
	state.Progress = &Progress{PlanID: p.ID, Plan: &p, Started: map[string]bool{}, Completed: map[string]Evidence{}}
	if err = e.Store.Write(state); err != nil {
		return err
	}
	return e.Apply(ctx, p)
}
