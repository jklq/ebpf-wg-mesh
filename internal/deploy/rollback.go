package deploy

import (
	"context"
	"fmt"
	"time"
)

// Rollback reverses software handovers, not database writes; schema changes require forward repair.
func (e Engine) Rollback(ctx context.Context) error {
	state, err := e.Store.Read()
	if err != nil {
		return err
	}
	if state.Progress == nil || state.Progress.Plan == nil {
		return fmt.Errorf("no interrupted upgrade to roll back")
	}
	failed := *state.Progress.Plan
	if failed.Rollback {
		return e.Apply(ctx, failed)
	}
	if !failed.RollingUpgrade() || failed.Previous == nil {
		return fmt.Errorf("software rollback requires an interrupted rolling upgrade")
	}
	old := failed.Previous
	if old.Release.Schema != failed.Release.Schema || old.Release.ConsoleSchema != failed.Release.ConsoleSchema {
		return fmt.Errorf("expanded schemas must be repaired forward; rollback never restores or deletes acknowledged data")
	}
	observed := snapshot(state)
	observed.Progress = nil
	observed.Policy = &failed.Installation
	observed.Bundle = &failed.Release
	observed.Placements = failed.Placements
	installation := snapshot(old.Installation)
	inv, err := e.Driver.Inventory(ctx, installation, old.Release, observed)
	if err != nil {
		return err
	}
	p, err := BuildPlan(installation, old.Release, observed, inv, false, time.Now())
	if err != nil {
		return err
	}
	if len(p.Unmet) > 0 {
		return fmt.Errorf("safe software rollback is unavailable: %v", p.Unmet)
	}
	p.Rollback = true
	p.Previous.Interrupted = &failed
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
