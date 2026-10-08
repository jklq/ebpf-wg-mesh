package deploy

import (
	"fmt"
	"time"
)

type RecoveryAdmission uint8

const (
	AuthorityAdmission RecoveryAdmission = iota
	FenceAdmission
	RuntimeAdmission
	ReportAdmission
	ApprovalAdmission
	PointAdmission
	CheckpointAdmission
)

// This interpreter is shared by the installer and offline host administration.
// Required admission facts live in HookContract, rather than separate resume
// implementations selecting their own verification obligations.
func validateAdmission(p Plan, r *RecoveryProgress, requirements []RecoveryAdmission, now time.Time) error {
	for _, requirement := range requirements {
		switch requirement {
		case AuthorityAdmission:
			if !p.Recovery || r == nil || r.Generation != p.Generation || r.Release != p.Release.ID {
				return fmt.Errorf("resume differs from the independently recorded recovery operation")
			}
		case FenceAdmission:
			if r == nil || !r.Fenced {
				return fmt.Errorf("recovery requires independently verified fencing")
			}
		case RuntimeAdmission:
			if r == nil || !r.Verified {
				return fmt.Errorf("recovery verification is incomplete; automation remains paused")
			}
		case ReportAdmission:
			if r == nil || r.Report == nil {
				return fmt.Errorf("recovery requires a concrete fleet report")
			}
			report := r.Report
			if report.Installation != p.Installation.ID || report.Generation != p.Generation || report.Release != p.Release.ID || report.Blocked || !report.Fenced {
				return fmt.Errorf("resume requires the fenced and resolved report for this recovery")
			}
		case ApprovalAdmission:
			if r == nil || r.Report == nil || r.Report.Blocked || r.ApprovedDigest != r.Report.ApprovalDigest() || r.ReservedDigest != r.ApprovedDigest {
				return ErrRecoveryApproval
			}
		case PointAdmission:
			if r == nil || r.Report == nil || r.NewRecoveryPoint == nil || r.NewRecoveryPoint.DataLossCutoff.Before(r.StartedAt) || !r.NewRecoveryPoint.DataLossCutoff.After(r.Report.Cutoff) {
				return fmt.Errorf("resume requires a new complete recovery point")
			}
			if err := validateRecoveryEvidence(*r.NewRecoveryPoint, p.Installation.ID, p.Installation.Backup.Target, now); err != nil {
				return err
			}
		case CheckpointAdmission:
			if r == nil || r.Report == nil {
				return fmt.Errorf("checkpoint admission requires fleet inventory")
			}
			if err := validateRecoveryCheckpoints(p, r.Report, r.Acknowledgements); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown recovery admission requirement %d", requirement)
		}
	}
	return nil
}
