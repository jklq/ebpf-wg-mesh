package deploy

import (
	"context"
	"fmt"
	"net/url"
	"strings"
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
	if restorationPlan(p) {
		if verifier, ok := e.Driver.(interface {
			VerifyRecovery(context.Context, Plan, State) error
		}); ok {
			if err := verifier.VerifyRecovery(ctx, p, state); err != nil {
				return err
			}
		}
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
		state.Generation = p.Generation
		state.Progress = &Progress{PlanID: p.ID, Plan: &p, Started: map[string]bool{}, Completed: map[string]Evidence{}}
		if err := e.Store.Write(state); err != nil {
			return err
		}
	}
	// An unfinished plan may have activated some or all processes, including an
	// interruption after recording resume but before committing the deployment.
	// Restore the pause and invalidate that activation before revisiting gates.
	for _, op := range p.Operations {
		if activationHook(op.Hook) && state.Progress.Started[op.ID] {
			if err := e.pauseBeforeRetry(ctx, p, state); err != nil {
				return err
			}
			delete(state.Progress.Completed, op.ID)
			if err := e.Store.Write(state); err != nil {
				return err
			}
		}
	}
	for _, op := range p.Operations {
		contract, _ := ContractForHook(op.Hook)
		for _, dependency := range op.Requires {
			if _, verified := state.Progress.Completed[dependency]; !verified {
				return fmt.Errorf("operation %s requires verified dependency %s", op.ID, dependency)
			}
		}
		if contract.Activation {
			if err := e.verifyActivationGates(ctx, p, state, contract); err != nil {
				if pauseErr := e.pauseBeforeRetry(ctx, p, state); pauseErr != nil {
					return fmt.Errorf("activation verification unresolved: %v; pause also unresolved: %w", err, pauseErr)
				}
				return fmt.Errorf("activation verification is unresolved; automation remains paused: %w", err)
			}
		}
		if op.Hook == "recovery-approve" {
			if err := e.recoveryApproval(ctx, p, &state, op); err != nil {
				return err
			}
			continue
		}
		if restorationPlan(p) {
			if err := requireRecoveryStep(p, state, op); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, ok := state.Progress.Completed[op.ID]; ok {
			critical := op.Kind == "hook" && contract.Reobserve
			if !critical && !contains([]string{"install", "database-join", "stage", "configure", "stage-tools"}, op.Kind) {
				continue
			}
			if !critical {
				done, _, err := e.Driver.Observe(ctx, p, state, op)
				if err != nil {
					return err
				}
				if done {
					continue
				}
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
				if activationHook(op.Hook) {
					if pauseErr := e.pauseBeforeRetry(ctx, p, state); pauseErr != nil {
						return fmt.Errorf("resume failed: %w; pause verification also failed: %v", err, pauseErr)
					}
				}
				return fmt.Errorf("execute %s/%s: %w", op.Kind, op.ID, err)
			}
			if binding.ServerID != "" {
				state.Bindings[op.Host] = binding
				if err := e.Store.Write(state); err != nil {
					return err
				}
			}
			done, evidence, err = e.Driver.Observe(ctx, p, state, op)
			if err != nil || !done {
				if activationHook(op.Hook) {
					if pauseErr := e.pauseBeforeRetry(ctx, p, state); pauseErr != nil {
						return fmt.Errorf("resume observation unresolved: %v; pause also unresolved: %w", err, pauseErr)
					}
				}
				if err != nil {
					return fmt.Errorf("verify %s/%s: %w", op.Kind, op.ID, err)
				}
				return fmt.Errorf("%s/%s is pending; resume this plan after native convergence", op.Kind, op.ID)
			}
		}
		if _, declared := ContractForHook(op.Hook); declared {
			if err := validateHookState(op.Hook, p, state, evidence, time.Now()); err != nil {
				if contract.Activation {
					if pauseErr := e.pauseBeforeRetry(ctx, p, state); pauseErr != nil {
						return fmt.Errorf("%w; pause unresolved: %v", err, pauseErr)
					}
				}
				return fmt.Errorf("verification contract %s: %w", op.Hook, err)
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
			state.LastBackup = evidence
		}
		if op.Hook == "recovery-verify" {
			state.LastRestore = &evidence
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
		if restorationPlan(p) {
			if err := recordRecoveryEvidence(p, &state, op, evidence, time.Now()); err != nil {
				return fmt.Errorf("%s: %w", op.Hook, err)
			}
			state.Recovery.Phase = op.Hook
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

func validateRecoveryEvidence(e Evidence, installation, target string, now time.Time) error {
	if e.Backup == "" || e.Point == nil || e.Object == nil || e.Object.Version == "" || e.Object.Version == "null" || e.Object.Digest != e.Point.ManifestDigest() || e.Object.RetainUntil.Before(e.Point.ExpiresAt) || !e.DataLossCutoff.Equal(e.Point.Snapshot.Timestamp) || e.Point.Installation != installation {
		return fmt.Errorf("backup verification requires a complete version-pinned recovery point at its reported cutoff")
	}
	u, err := url.Parse(e.Backup)
	destination, parseErr := url.Parse(target)
	if err != nil || parseErr != nil || u.Scheme != "s3" || u.Host != destination.Host || u.User != nil || u.Fragment != "" || len(u.Query()) != 1 || len(u.Query()["versionId"]) != 1 || u.Query().Get("versionId") != e.Object.Version || strings.TrimPrefix(u.Path, "/") != e.Object.Key || !strings.HasPrefix(e.Object.Key, strings.Trim(destination.Path, "/")+"/points/"+installation+"/") {
		return fmt.Errorf("backup URL must pin the verified catalog object in the declared independent recovery storage")
	}
	if err := e.Point.Validate(now); err != nil {
		return err
	}
	return e.Point.ValidateDependencies()
}

func (e Engine) pauseBeforeRetry(ctx context.Context, p Plan, state State) error {
	pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	op := Operation{Kind: "hook", Host: p.AdministrationHost, Hook: "quiesce", ID: "pause-interrupted-resume"}
	for _, candidate := range p.Operations {
		if candidate.Hook == "quiesce" {
			op = candidate
			break
		}
	}
	if _, err := e.Driver.Execute(pauseCtx, p, state, op); err != nil {
		return fmt.Errorf("interrupted resume requires a verified pause: %w", err)
	}
	done, evidence, err := e.Driver.Observe(pauseCtx, p, state, op)
	if err != nil || !done {
		return fmt.Errorf("pause remains unresolved after interrupted resume: %v", err)
	}
	if err := validateHookState(op.Hook, p, state, evidence, time.Now()); err != nil {
		return fmt.Errorf("interrupted resume pause verification contract: %w", err)
	}
	return nil
}

func activationHook(name string) bool {
	c, _ := ContractForHook(name)
	return c.Activation
}

func (e Engine) verifyActivationGates(ctx context.Context, p Plan, state State, contract HookContract) error {
	for _, name := range contract.LiveGates {
		var gate *Operation
		for _, op := range p.Operations {
			if op.Hook == name {
				selected := op
				gate = &selected
			}
		}
		if gate == nil {
			return fmt.Errorf("required activation gate %s is missing", name)
		}
		done, evidence, err := e.Driver.Observe(ctx, p, state, *gate)
		if err != nil || !done {
			return fmt.Errorf("live %s verification failed: %v", name, err)
		}
		if err := validateHookState(name, p, state, evidence, time.Now()); err != nil {
			return err
		}

	}
	return nil
}
