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
			if _, ok := i.Host(pl.Host); ok && inv.Hosts[pl.Host].Online {
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
	// Recovery always uses a new database store; occupied stores remain intact.
	for n := range p.Placements {
		if p.Placements[n].Role == Database {
			p.Placements[n].Instance = fmt.Sprintf("cockroachdb-%d-%s", p.Placements[n].Ordinal, Digest([]any{p.Generation, p.Placements[n].Host})[:12])
		}
	}
	groups := map[string][]Operation{}
	add := func(phase, kind, host, hook string, pl *Placement) {
		groups[phase] = append(groups[phase], Operation{Kind: kind, Host: host, Hook: hook, Placement: pl})
	}
	for _, op := range p.Operations {
		if !contains([]string{"stage", "stage-tools", "purchase"}, op.Kind) {
			continue
		}
		if op.Placement != nil {
			for _, pl := range p.Placements {
				if pl.Slot() == op.Placement.Slot() {
					op.Placement = &pl
					break
				}
			}
		}
		groups[op.Phase] = append(groups[op.Phase], op)
	}
	for _, pl := range state.Placements {
		if pl.Role != Agent {
			if _, present := i.Host(pl.Host); present {
				add("stop-prior", "stop", pl.Host, "", &pl)
			}
		}
	}
	for _, pl := range p.Placements {
		phase := string(pl.Role)
		if pl.Role == Database {
			phase = "database-runtime"
		}
		for _, action := range runtimeActions {
			add(phase, action, pl.Host, "", &pl)
		}
	}
	ops, err := p.compileWorkflow(groups)
	if err != nil {
		return p, state, err
	}
	p.Operations, p.DatabaseChanges = ops, nil
	for _, op := range ops {
		if op.Hook == "database-verify" {
			p.DatabaseChanges = append(p.DatabaseChanges, op)
		}
	}
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
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
			return err
		}
		r.Fenced = true
	case "recovery-database":
		return ValidateHookEvidence(op.Hook, p, e)
	case "restore":
		if !r.Fenced {
			return fmt.Errorf("restore requires independently verified fencing")
		}
		return ValidateHookEvidence(op.Hook, p, e)
	case "recovery-authority":
		return ValidateHookEvidence(op.Hook, p, e)
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
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
			return err
		}
		if r.Report == nil || e.Recovery.ReportDigest != r.Report.ApprovalDigest() {
			return fmt.Errorf("reservation evidence does not match inventory report")
		}
		r.ReservedDigest = e.Recovery.ReportDigest
	case "recovery-reconcile":
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
			return err
		}
		if r.Report == nil || r.ApprovedDigest != r.Report.ApprovalDigest() || e.Recovery.ReportDigest != r.ApprovedDigest {
			return ErrRecoveryApproval
		}
	case "recovery-checkpoints":
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
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
		return ValidateHookEvidence(op.Hook, p, e)
	case "production-verify":
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
			return err
		}
		r.Verified = true
	case "backup":
		point := e
		point.Recovery, point.Fleet = nil, nil
		r.NewRecoveryPoint = &point
	case "recovery-resume":
		if err := validateRecoveryResume(p, r, now); err != nil {
			return err
		}
		if err := ValidateHookEvidence(op.Hook, p, e); err != nil {
			return err
		}
		r.CompletedAt = now.UTC()
	}
	return nil
}

// Check before executing a hook: checking its receipt after a resume or
// destructive reconciliation has run would be too late to enforce the gate.
func requireRecoveryStep(p Plan, state State, op Operation) error {
	contract, _ := ContractForHook(op.Hook)
	r := state.Recovery
	if r == nil || r.Generation != p.Generation {
		return fmt.Errorf("recovery generation is not independently recorded")
	}
	return validateAdmission(p, r, contract.Admission, time.Now())
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
	contract, _ := ContractForHook("recovery-resume")
	return validateAdmission(p, r, contract.Admission, now)
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
