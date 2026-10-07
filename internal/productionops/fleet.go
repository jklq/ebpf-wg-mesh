package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/recovery"
)

func (r *Runner) fleetPath() string {
	return filepath.Join(r.Config.StateDirectory, r.Plan.ID, "fleet.json")
}
func (r *Runner) captureFleet(ctx context.Context) error {
	if !r.Plan.Recovery {
		return fmt.Errorf("inventory requires recovery")
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	input := recovery.FleetInput{CapturedAt: time.Now().UTC()}
	input.Desired, err = recovery.ReadDesiredFleet(ctx, db)
	if err != nil {
		return err
	}
	input.DesiredResources, err = recovery.ReadDesiredResources(ctx, db)
	if err != nil {
		return err
	}
	input.DesiredNetworks, err = recovery.ReadDesiredNetworks(ctx, db)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent {
			continue
		}
		b, err := r.hostProbe(ctx, r.Plan, pl, r.expandProbe(r.Config.Probes[deploy.Agent], pl), false)
		if err != nil {
			if err := r.verifyHostFenced(ctx, r.Plan, pl.Host); err != nil {
				return fmt.Errorf("agent is unreachable and lacks a fence: %w", err)
			}
			// Last snapshot identities and reservations are retained when an explicitly
			// fenced machine cannot report. No allocation is scheduled onto that machine.
			for _, h := range input.Desired {
				if h.ID == pl.Instance {
					h.Reachable = false
					h.Isolated = true
					input.Observed = append(input.Observed, h)
					seen[h.ID] = true
				}
			}
			continue
		}
		var report health.Report
		if err := json.Unmarshal(b, &report); err != nil {
			return err
		}
		var h recovery.FleetHost
		if err := json.Unmarshal(report.Inventory, &h); err != nil {
			return fmt.Errorf("agent must report its complete durable and runtime inventory: %w", err)
		}
		if h.ID != pl.Instance || h.Generation != r.Plan.Generation || report.Authority == nil || !report.Authority.Paused || report.Authority.Generation != r.Plan.Generation {
			return fmt.Errorf("inventory differs from live admitted identity")
		}
		input.Observed = append(input.Observed, h)
		seen[h.ID] = true
	}
	// SQL registrations outside the managed placements cannot be silently ignored.
	// They need an applied host binding or independently confirmed provider fence.
	prior, err := r.priorPlan()
	if err != nil {
		return err
	}
	for _, h := range input.Desired {
		if seen[h.ID] {
			continue
		}
		found := false
		for _, pl := range prior.Placements {
			if pl.Role == deploy.Agent && pl.Instance == h.ID {
				if err := r.verifyHostFenced(ctx, prior, pl.Host); err != nil {
					return err
				}
				h.Isolated = true
				h.Reachable = false
				input.Observed = append(input.Observed, h)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("registered agent %s has no applied external host binding; add its Linux host before recovery", h.ID)
		}
	}
	return saveJSON(r.fleetPath(), input)
}
func (r *Runner) readFleet(ctx context.Context) (*recovery.FleetInput, error) {
	var f recovery.FleetInput
	if err := privateJSON(r.fleetPath(), &f); err != nil {
		return nil, err
	}
	if r.RecoveryProgress == nil || f.CapturedAt.Before(r.RecoveryProgress.StartedAt) {
		return nil, fmt.Errorf("inventory predates recovery")
	}
	return &f, nil
}
func (r *Runner) report(approved bool) (*recovery.FleetReport, error) {
	progress := r.RecoveryProgress
	if progress == nil || progress.Report == nil || progress.Generation != r.Plan.Generation {
		return nil, fmt.Errorf("recorded fleet report is required")
	}
	report := progress.Report
	if report.Generation != r.Plan.Generation || report.Installation != r.Plan.Installation.ID {
		return nil, fmt.Errorf("fleet report differs from operation")
	}
	if approved && (report.Blocked || progress.ApprovedDigest != report.ApprovalDigest() || progress.ReservedDigest != report.ApprovalDigest()) {
		return nil, fmt.Errorf("fleet report requires approval and verified reservations")
	}
	return report, nil
}
func (r *Runner) reserveFleet(ctx context.Context, verify bool) error {
	report, err := r.report(false)
	if err != nil {
		return err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if !verify {
		if err := recovery.ReserveFleet(ctx, db, *report); err != nil {
			return err
		}
	}
	for _, reservation := range report.Reservations {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recovery_network_reservations WHERE generation=$1 AND owner=$2 AND environment_id=$3 AND network_identity=$4 AND prefix=$5`, r.Plan.Generation, reservation.Owner, reservation.EnvironmentID, int64(reservation.Identity), reservation.Prefix).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("network reservation missing")
		}
		if reservation.Identity != 0 {
			var next int64
			if err := db.QueryRowContext(ctx, `SELECT next_identity FROM environment_network_identity_counter WHERE id=TRUE`).Scan(&next); err != nil {
				return err
			}
			if next <= int64(reservation.Identity) {
				return fmt.Errorf("network allocator has not advanced beyond retained identities")
			}
		}
	}
	return nil
}
func (r *Runner) reconcileFleet(ctx context.Context, verify bool) error {
	if _, err := r.report(true); err != nil {
		return err
	}
	if !verify {
		if err := r.reserveFleet(ctx, true); err != nil {
			return err
		}
		if err := r.restoreArtifacts(ctx, false); err != nil {
			return err
		}
		return r.installAdmission(ctx, true, true, true)
	}
	// Complete restored checkpoints perform adoption/recreation. Host admission
	// keeps all cleanup paused, including resources unknown at the cutoff.
	if err := r.verifyPause(ctx, true, true); err != nil {
		return err
	}
	if err := r.restoreArtifacts(ctx, true); err != nil {
		return err
	}
	return r.verifyQuarantine(ctx)
}
func (r *Runner) verifyQuarantine(ctx context.Context) error {
	before, err := r.readFleet(ctx)
	if err != nil {
		return err
	}
	for _, old := range before.Observed {
		if !old.Reachable {
			continue
		}
		var pl deploy.Placement
		found := false
		for _, candidate := range r.Plan.Placements {
			if candidate.Role == deploy.Agent && candidate.Instance == old.ID {
				pl = candidate
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("observed host disappeared from recovery inventory")
		}
		b, err := r.hostProbe(ctx, r.Plan, pl, r.expandProbe(r.Config.Probes[deploy.Agent], pl), false)
		if err != nil {
			return err
		}
		var report health.Report
		if err := json.Unmarshal(b, &report); err != nil {
			return err
		}
		var current recovery.FleetHost
		if err := json.Unmarshal(report.Inventory, &current); err != nil {
			return err
		}
		// Runtime inventory includes quarantined containers and volumes. Unknown
		// identities are preserved until an explicit independent resolution plan.
		for _, resource := range old.Resources {
			if !slices.ContainsFunc(current.Resources, func(v recovery.FleetResource) bool { return v.ID == resource.ID && v.Kind == resource.Kind }) {
				return fmt.Errorf("quarantined resource %s disappeared", resource.ID)
			}
		}
		for _, allocation := range old.Allocations {
			if !slices.ContainsFunc(current.Allocations, func(v recovery.FleetAllocation) bool { return v.ID == allocation.ID }) {
				return fmt.Errorf("retained allocation %s disappeared", allocation.ID)
			}
		}
	}
	return nil
}
func (r *Runner) checkpoints(ctx context.Context, verify bool) error {
	if _, err := r.report(true); err != nil {
		return err
	}
	if !verify {
		if err := r.installAdmission(ctx, true, true, true); err != nil {
			return err
		}
	}
	_, err := r.verifyCheckpoints(ctx)
	return err
}
func (r *Runner) verifyCheckpoints(ctx context.Context) (map[string]deploy.CheckpointAcknowledgement, error) {
	report, err := r.report(true)
	if err != nil {
		return nil, err
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var epoch uint64
	if err := db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id=1`).Scan(&epoch); err != nil {
		return nil, err
	}
	result := map[string]deploy.CheckpointAcknowledgement{}
	expected := append([]string{}, report.AdmittedAgents...)
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Agent {
			expected = append(expected, pl.Instance)
		}
	}
	slices.Sort(expected)
	expected = slices.Compact(expected)
	for _, id := range expected {
		var pl deploy.Placement
		for _, v := range r.Plan.Placements {
			if v.Role == deploy.Agent && v.Instance == id {
				pl = v
			}
		}
		if pl.Instance == "" {
			return nil, fmt.Errorf("agent %s lacks an external binding", id)
		}
		probe := r.expandProbe(r.Config.Probes[deploy.Agent], pl)
		if probe.RuntimeURL != "" {
			probe.URL = probe.RuntimeURL
		}
		b, err := r.hostProbe(ctx, r.Plan, pl, probe, false)
		if err != nil {
			return nil, err
		}
		var live health.Report
		if err := json.Unmarshal(b, &live); err != nil {
			return nil, err
		}
		var desired int64
		if err := db.QueryRowContext(ctx, `SELECT desired_revision FROM agent_registrations WHERE id=$1`, id).Scan(&desired); err != nil {
			return nil, err
		}
		ack := live.Checkpoint
		if ack == nil || ack.Generation != r.Plan.Generation || !ack.Complete || ack.AuthorityEpoch != epoch || ack.Cursor < desired || live.Authority == nil || !live.Authority.Paused || !live.Authority.Checkpoints {
			return nil, fmt.Errorf("agent %s has not durably accepted its complete restored checkpoint", id)
		}
		result[id] = deploy.CheckpointAcknowledgement{Generation: ack.Generation, AuthorityEpoch: ack.AuthorityEpoch, Cursor: ack.Cursor, Complete: true}
	}
	if err := r.verifyIngress(ctx); err != nil {
		return nil, err
	}
	if err := r.verifyQuarantine(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
