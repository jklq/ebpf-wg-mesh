package deploy

import (
	"context"
	"fmt"
	"time"
)

type Driver interface {
	Inventory(context.Context, Installation, Release, State) (Inventory, error)
	// Observe must discover uncertain provider outcomes and verify native
	// membership/drain/backup completion before an operation is considered done.
	Observe(context.Context, Plan, State, Operation) (bool, Evidence, error)
	Execute(context.Context, Plan, State, Operation) (Binding, error)
}

type Engine struct {
	Store  *Store
	Driver Driver
}

func (e Engine) Apply(ctx context.Context, p Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if len(p.Unmet) > 0 {
		return fmt.Errorf("plan has unmet placement requirements: %v", p.Unmet)
	}
	state, err := e.Store.Read()
	if err != nil {
		return err
	}
	if state.Progress != nil && state.Progress.PlanID != p.ID {
		return fmt.Errorf("operation %s is interrupted; resume its original plan before applying another", state.Progress.PlanID)
	}
	if state.Progress == nil {
		if state.Revision != p.StateRevision || Digest(state) != p.StateDigest {
			return fmt.Errorf("materially stale plan: deployment state changed")
		}
		inv, err := e.Driver.Inventory(ctx, p.Installation, p.Release, state)
		if err != nil {
			return err
		}
		if Digest(inv) != p.InventoryDigest {
			return fmt.Errorf("materially stale plan: observed inventory changed")
		}
		state.InstallationID = p.Installation.ID
		state.Progress = &Progress{PlanID: p.ID, Plan: &p, Started: map[string]bool{}, Completed: map[string]Evidence{}}
		if err := e.Store.Write(state); err != nil {
			return err
		}
	}
	for _, op := range p.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := state.Progress.Completed[op.ID]; ok {
			if !contains([]string{"install", "database-join", "stage", "configure", "stage-tools"}, op.Kind) {
				continue
			}
			done, _, err := e.Driver.Observe(ctx, p, state, op)
			if err != nil {
				return err
			}
			if done {
				continue
			}
			delete(state.Progress.Completed, op.ID)
		}
		done, evidence, err := e.Driver.Observe(ctx, p, state, op)
		if err != nil {
			return fmt.Errorf("observe %s/%s: %w", op.Kind, op.ID, err)
		}
		if !done {
			if op.Kind == "purchase" && state.Progress.Started[op.ID] {
				return fmt.Errorf("purchase outcome for %s is unknown after discovery; resolve the provider binding before resuming, never blindly repeat a purchase", op.Host)
			}
			state.Progress.Started[op.ID] = true
			if err := e.Store.Write(state); err != nil {
				return err
			}
			binding, err := e.Driver.Execute(ctx, p, state, op)
			if err != nil {
				return fmt.Errorf("execute %s/%s: %w", op.Kind, op.ID, err)
			}
			if binding.ServerID != "" {
				state.Bindings[op.Host] = binding
				if err := e.Store.Write(state); err != nil {
					return err
				}
			}
			done, evidence, err = e.Driver.Observe(ctx, p, state, op)
			if err != nil {
				return fmt.Errorf("verify %s/%s: %w", op.Kind, op.ID, err)
			}
			if !done {
				return fmt.Errorf("%s/%s is pending; resume this plan after native convergence", op.Kind, op.ID)
			}
		}
		if op.Kind == "purchase" && state.Bindings[op.Host].ServerID == "" {
			inv, err := e.Driver.Inventory(ctx, p.Installation, p.Release, state)
			if err != nil {
				return err
			}
			h, _ := p.Installation.Host(op.Host)
			id := inv.Hosts[op.Host].ServerID
			if id == "" {
				return fmt.Errorf("discovered purchase has no binding")
			}
			state.Bindings[op.Host] = Binding{Provider: h.Binding.Provider, ServerID: id}
		}
		if op.Hook == "backup" {
			if evidence.Backup == "" || evidence.DataLossCutoff.IsZero() {
				return fmt.Errorf("complete backup must report its location and data-loss cutoff")
			}
			if time.Since(evidence.DataLossCutoff) > time.Duration(p.Installation.Backup.MaxAgeHours)*time.Hour {
				return fmt.Errorf("verified backup is older than the applied backup policy")
			}
			state.LastBackup = evidence
		}
		if op.Kind == "retain" && op.Placement != nil {
			found := false
			for _, pl := range state.Retained {
				if pl.Instance == op.Placement.Instance {
					found = true
				}
			}
			if !found {
				state.Retained = append(state.Retained, *op.Placement)
			}
		}
		if op.Kind == "retire" && op.Placement != nil {
			out := state.Retained[:0]
			for _, pl := range state.Retained {
				if pl.Instance != op.Placement.Instance {
					out = append(out, pl)
				}
			}
			state.Retained = out
		}
		state.Progress.Completed[op.ID] = evidence
		if op.Kind == "delete" {
			if state.Deleted == nil {
				state.Deleted = map[string]bool{}
			}
			state.Deleted[op.Host] = true
			delete(state.Bindings, op.Host)
		}
		if err := e.Store.Write(state); err != nil {
			return err
		}
	}
	state.Policy = &p.Installation
	state.Bundle = &p.Release
	state.Placements = p.Placements
	for _, h := range p.Installation.Hosts {
		if !state.Deleted[h.ID] && state.Bindings[h.ID].ServerID == "" && h.Binding.ServerID != "" {
			state.Bindings[h.ID] = h.Binding
		}
	}
	state.Progress = nil
	state.Revision++
	return e.Store.Write(state)
}
