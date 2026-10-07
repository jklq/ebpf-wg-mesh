package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"ebof-wg-mesh/internal/recovery"
	"github.com/google/uuid"
)

var ErrRecoveryApproval = errors.New("recovery reconciliation report requires operator approval")

type RecoveryProgress struct {
	NewRecoveryPoint *Evidence                            `json:"newRecoveryPoint,omitempty"`
	ReportDigest     string                               `json:"reportDigest,omitempty"`
	ElapsedSeconds   float64                              `json:"elapsedSeconds"`
	Generation       string                               `json:"generation"`
	StartedAt        time.Time                            `json:"startedAt"`
	Release          string                               `json:"release"`
	Phase            string                               `json:"phase"`
	Fenced           bool                                 `json:"fenced"`
	Report           *recovery.FleetReport                `json:"report,omitempty"`
	ApprovedDigest   string                               `json:"approvedDigest,omitempty"`
	ReservedDigest   string                               `json:"reservedDigest,omitempty"`
	Acknowledgements map[string]CheckpointAcknowledgement `json:"acknowledgements,omitempty"`
	Verified         bool                                 `json:"verified"`
	CompletedAt      time.Time                            `json:"completedAt,omitempty"`
}

type CheckpointAcknowledgement struct {
	Generation     string `json:"generation"`
	AuthorityEpoch uint64 `json:"authorityEpoch"`
	Cursor         int64  `json:"cursor"`
	Complete       bool   `json:"complete"`
}

type RecoveryReceipt struct {
	Installation     string                               `json:"installation"`
	Generation       string                               `json:"generation"`
	ReportDigest     string                               `json:"reportDigest,omitempty"`
	Checks           map[string]bool                      `json:"checks"`
	Acknowledgements map[string]CheckpointAcknowledgement `json:"acknowledgements,omitempty"`
}

func newRestorePlan(i Installation, r Release, state State, inv Inventory, selected Evidence, now time.Time) (Plan, State, error) {
	// Preserve durable surviving agent identities, but re-place all infrastructure
	// against the new target. Infrastructure count and provider may change.
	base := State{Version: 1, Bindings: state.Bindings}
	for _, pl := range state.Placements {
		if pl.Role == Agent {
			if _, ok := i.Host(pl.Host); ok {
				base.Placements = append(base.Placements, pl)
			}
		}
	}
	p, err := BuildPlan(i, r, base, inv, false, now)
	if err != nil {
		return p, state, err
	}
	p.Previous = previousDeployment(state)
	p.Recovery = true
	p.StateRevision = state.Revision
	p.Generation = uuid.NewString()
	var ops []Operation
	add := func(kind, host, hook string, pl *Placement) {
		op := Operation{Kind: kind, Host: host, Hook: hook, Placement: pl}
		op.ID = Digest([]any{op, len(ops)})[:24]
		ops = append(ops, op)
	}
	for _, op := range p.Operations {
		if contains([]string{"stage", "stage-tools", "purchase"}, op.Kind) {
			ops = append(ops, op)
		}
	}
	add("hook", p.AdministrationHost, "recovery-verify", nil)
	add("hook", p.AdministrationHost, "recovery-fence", nil)
	for _, pl := range state.Placements {
		if pl.Role != Agent {
			if _, present := i.Host(pl.Host); present {
				add("stop", pl.Host, "", &pl)
			}
		}
	}
	for _, pl := range p.Placements {
		if pl.Role == Database {
			add("configure", pl.Host, "", &pl)
			add("install", pl.Host, "", &pl)
		}
	}
	add("hook", p.AdministrationHost, "recovery-database", nil)
	add("hook", p.AdministrationHost, "restore", nil)
	add("hook", p.AdministrationHost, "recovery-authority", nil)
	for _, role := range []Role{Registry, ControlPlane, Console, Agent, Envoy, Builder} {
		for _, pl := range p.Placements {
			if pl.Role == role {
				add("configure", pl.Host, "", &pl)
				add("install", pl.Host, "", &pl)
			}
		}
	}
	for _, hook := range []string{"recovery-inventory", "recovery-reserve", "recovery-approve", "recovery-reconcile", "recovery-checkpoints", "recovery-work", "database-verify", "storage-verify", "production-verify", "backup-schedule", "backup", "recovery-resume"} {
		add("hook", p.AdministrationHost, hook, nil)
	}
	p.Operations = ops
	if len(p.Unmet) > 0 {
		return p, state, fmt.Errorf("recovery inventory has unmet placement requirements: %v", p.Unmet)
	}
	state.Version = 1
	state.LastRestore = &selected
	state.InstallationID = i.ID
	state.Generation = p.Generation
	state.Recovery = &RecoveryProgress{Generation: p.Generation, StartedAt: now.UTC(), Release: r.ID, Phase: "declared"}
	state.Progress = nil
	p.StateDigest = Digest(state)
	p.ID = p.digest()
	state.Progress = &Progress{PlanID: p.ID, Plan: &p, Started: map[string]bool{}, Completed: map[string]Evidence{}}
	return p, state, nil
}

func (e Engine) ApproveRecovery(digest string) error {
	state, err := e.Store.Read()
	if err != nil {
		return err
	}
	r := state.Recovery
	if r == nil || r.Report == nil || state.Progress == nil || r.Report.ApprovalDigest() != digest {
		return fmt.Errorf("approval must name the current concrete recovery report digest")
	}
	if r.Report.Blocked || r.ReservedDigest != digest {
		return fmt.Errorf("resolve authority/network conflicts and verify reservations before approval")
	}
	r.ApprovedDigest = digest
	return e.Store.Write(state)
}

func validateReceipt(e Evidence, p Plan, checks ...string) error {
	r := e.Recovery
	if r == nil || r.Installation != p.Installation.ID || r.Generation != p.Generation {
		return fmt.Errorf("recovery hook must return evidence for this installation and generation")
	}
	for _, check := range checks {
		if !r.Checks[check] {
			return fmt.Errorf("recovery verification missing %s", check)
		}
	}
	return nil
}

func recordRecoveryEvidence(p Plan, state *State, op Operation, e Evidence, now time.Time) error {
	r := state.Recovery
	if r == nil {
		return fmt.Errorf("restore has no independently recorded recovery operation")
	}
	switch op.Hook {
	case "recovery-fence":
		if err := validateReceipt(e, p, "provider-or-host-fence", "credentials-replaced"); err != nil {
			return err
		}
		r.Fenced = true
	case "recovery-database":
		return validateReceipt(e, p, "empty-destination", "schemas-not-initialized")
	case "restore":
		if !r.Fenced {
			return fmt.Errorf("restore requires independently verified fencing")
		}
		return validateReceipt(e, p, "database-restored", "selected-release")
	case "recovery-authority":
		return validateReceipt(e, p, "new-ca-without-overlap", "client-identities", "console-sessions-invalidated", "registry-authority", "all-participants-paused", "host-admin-admission")
	case "recovery-inventory":
		if e.Fleet == nil {
			return fmt.Errorf("recovery inventory must return restored desired state and latest external host inventory")
		}
		report, err := recovery.CompareFleet(*e.Fleet, p.Installation.ID, p.Generation, p.Release.ID, state.LastRestore.DataLossCutoff, r.StartedAt, now, r.Fenced)
		if err != nil {
			return err
		}
		r.Report = &report
		r.ReportDigest = report.ApprovalDigest()
	case "recovery-reserve":
		if err := validateReceipt(e, p, "network-reservations"); err != nil {
			return err
		}
		if r.Report == nil || e.Recovery.ReportDigest != r.Report.ApprovalDigest() {
			return fmt.Errorf("reservation evidence does not match inventory report")
		}
		r.ReservedDigest = e.Recovery.ReportDigest
	case "recovery-reconcile":
		if err := validateReceipt(e, p, "quarantine-preserved", "approved-desired-state"); err != nil {
			return err
		}
		if r.Report == nil || r.ApprovedDigest != r.Report.ApprovalDigest() || e.Recovery.ReportDigest != r.ApprovedDigest {
			return ErrRecoveryApproval
		}
	case "recovery-checkpoints":
		if err := validateReceipt(e, p, "complete-checkpoints"); err != nil {
			return err
		}
		if r.Report == nil {
			return fmt.Errorf("checkpoint verification requires fleet report")
		}
		if err := validateRecoveryCheckpoints(p, r.Report, e.Recovery.Acknowledgements); err != nil {
			return err
		}
		r.Acknowledgements = e.Recovery.Acknowledgements
	case "recovery-work":
		return validateReceipt(e, p, "stale-build-ownership-invalidated", "new-worker-leases", "external-effects-reconciled")
	case "production-verify":
		if err := validateReceipt(e, p, "database-health", "key-access", "overlay-connectivity", "image-access", "ingress-acknowledgements", "certificate-trust", "console-login"); err != nil {
			return err
		}
		r.Verified = true
	case "backup":
		if e.DataLossCutoff.Before(r.StartedAt) || !e.DataLossCutoff.After(state.LastRestore.DataLossCutoff) {
			return fmt.Errorf("resume requires a new complete recovery point")
		}
		point := e
		point.Recovery, point.Fleet = nil, nil
		r.NewRecoveryPoint = &point
	case "recovery-resume":
		if err := validateRecoveryResume(p, r, now); err != nil {
			return err
		}
		if err := validateReceipt(e, p, "all-participants-resumed"); err != nil {
			return err
		}
		r.CompletedAt = now.UTC()
	}
	return nil
}

// Check before executing a hook: checking its receipt after a resume or
// destructive reconciliation has run would be too late to enforce the gate.
func requireRecoveryStep(p Plan, state State, op Operation) error {
	r := state.Recovery
	if r == nil || r.Generation != p.Generation {
		return fmt.Errorf("recovery generation is not independently recorded")
	}
	if op.Hook == "restore" && !r.Fenced {
		return fmt.Errorf("restore requires independent fencing before execution")
	}
	if contains([]string{"recovery-reconcile", "recovery-checkpoints", "recovery-work", "recovery-resume"}, op.Hook) {
		if r.Report == nil || r.Report.Blocked || r.ApprovedDigest != r.Report.ApprovalDigest() || r.ReservedDigest != r.ApprovedDigest {
			return ErrRecoveryApproval
		}
	}
	if op.Hook == "recovery-resume" {
		return validateRecoveryResume(p, r, time.Now())
	}
	return nil
}

func validateRecoveryCheckpoints(p Plan, report *recovery.FleetReport, acks map[string]CheckpointAcknowledgement) error {
	admitted := append([]string{}, report.AdmittedAgents...)
	for _, placement := range p.Placements {
		if placement.Role == Agent {
			admitted = append(admitted, placement.Instance)
		}
	}
	slices.Sort(admitted)
	for _, agent := range slices.Compact(admitted) {
		ack, ok := acks[agent]
		if !ok || ack.Generation != p.Generation || !ack.Complete || ack.AuthorityEpoch == 0 || ack.Cursor < 0 {
			return fmt.Errorf("agent %s has not acknowledged a complete checkpoint in the selected generation", agent)
		}
	}
	return nil
}

// The controller and host-admin SQL transition enforce the same resume contract.
func validateRecoveryResume(p Plan, r *RecoveryProgress, now time.Time) error {
	if !p.Recovery || r == nil || r.Generation != p.Generation || r.Release != p.Release.ID {
		return fmt.Errorf("resume differs from the independently recorded recovery operation")
	}
	if !r.Fenced || !r.Verified || r.Report == nil {
		return fmt.Errorf("recovery verification is incomplete; automation remains paused")
	}
	report := r.Report
	if report.Installation != p.Installation.ID || report.Generation != p.Generation || report.Release != p.Release.ID || report.Blocked || !report.Fenced {
		return fmt.Errorf("resume requires the fenced and resolved report for this recovery")
	}
	if r.ApprovedDigest != report.ApprovalDigest() || r.ReservedDigest != r.ApprovedDigest {
		return ErrRecoveryApproval
	}
	if r.NewRecoveryPoint == nil || r.NewRecoveryPoint.DataLossCutoff.Before(r.StartedAt) || !r.NewRecoveryPoint.DataLossCutoff.After(report.Cutoff) {
		return fmt.Errorf("resume requires a new complete recovery point")
	}
	if err := validateRecoveryEvidence(*r.NewRecoveryPoint, p.Installation.ID, p.Installation.Backup.Target, now); err != nil {
		return err
	}
	return validateRecoveryCheckpoints(p, report, r.Acknowledgements)
}

func (e Engine) RefreshRecoveryInventory() error {
	state, err := e.Store.Read()
	if err != nil {
		return err
	}
	if state.Recovery == nil || state.Progress == nil || state.Progress.Plan == nil {
		return fmt.Errorf("no interrupted recovery inventory to refresh")
	}
	for _, op := range state.Progress.Plan.Operations {
		if op.Hook == "recovery-reconcile" && state.Progress.Started[op.ID] {
			return fmt.Errorf("reconciliation has started; resume its approved report before refreshing inventory")
		}
	}
	refresh := false
	for _, op := range state.Progress.Plan.Operations {
		refresh = refresh || op.Hook == "recovery-inventory"
		if refresh {
			delete(state.Progress.Completed, op.ID)
			delete(state.Progress.Started, op.ID)
		}
	}
	state.Recovery.Report = nil
	state.Recovery.ReportDigest = ""
	state.Recovery.ApprovedDigest, state.Recovery.ReservedDigest = "", ""
	return e.Store.Write(state)
}

func (e Engine) recoveryApproval(ctx context.Context, p Plan, state *State, op Operation) error {
	_ = ctx
	r := state.Recovery
	if r == nil || r.Report == nil {
		return fmt.Errorf("recovery has no concrete fleet report")
	}
	digest := r.Report.ApprovalDigest()
	if r.ApprovedDigest != digest {
		return fmt.Errorf("%w: %s; inspect platformctl status, then resume --approve-report %s", ErrRecoveryApproval, digest, digest)
	}
	if r.Report.Blocked || r.ReservedDigest != digest {
		return fmt.Errorf("recovery report has unresolved fencing, authority or network reservations")
	}
	state.Progress.Completed[op.ID] = Evidence{}
	r.Phase = op.Hook
	return e.Store.Write(*state)
}
