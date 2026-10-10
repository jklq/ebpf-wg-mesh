package deploy

import (
	"ebof-wg-mesh/internal/recovery"
	"fmt"
	"sort"
	"time"
)

// HookContract is code-owned policy shared by the planner, the execution engine
// and the native verifier. It describes evidence and admission, not algorithms.
// A receipt is produced only after the corresponding native inspection succeeds.
type HookContract struct {
	Kind       VerificationKind
	PointScope PointScope
	Checks     []string
	Reobserve  bool
	Activation bool
	LiveGates  []string
	Admission  []RecoveryAdmission
}

type VerificationKind uint8

const (
	ReceiptVerification VerificationKind = iota
	DatabaseVerification
	StorageVerification
	PointVerification
	FleetVerification
)

type PointScope uint8

const (
	UnboundPoint PointScope = iota
	SelectedPoint
	SuccessorPoint
)

var hookContracts = map[string]HookContract{
	"database-credentials": {Checks: []string{"database-tls-provisioned"}},
	"database-init":        {Checks: []string{"fresh-database-admitted"}},
	"platform-bootstrap":   {Checks: []string{"platform-bootstrap-ready"}},
	"credentials":          {Checks: []string{"component-identities-provisioned"}},
	"reservations":         {Checks: []string{"scheduler-reservations-admitted"}},
	"quiesce":              {Checks: []string{"all-participants-paused", "prior-maintenance-disabled"}},
	"resume":               {Activation: true, LiveGates: []string{"production-verify"}, Checks: []string{"all-participants-resumed"}},
	"recovery-protect":     {Reobserve: true, Checks: []string{"complete-dependencies-protected"}},
	"backup-schedule":      {Reobserve: true, Checks: []string{"independent-schedules-admitted"}},
	"backup":               {Reobserve: true, Kind: PointVerification, PointScope: SuccessorPoint},
	"recovery-finalize":    {Checks: []string{"applied-release-finalized"}},
	"database-verify":      {Kind: DatabaseVerification},
	"database-upgrade":     {Kind: DatabaseVerification},
	"database-finalize":    {Checks: []string{"database-version-finalized"}},
	"builder-drain":        {Checks: []string{"builder-work-drained"}},
	"builder-resume":       {Checks: []string{"builder-drain-restored"}},
	"rolling-prepare":      {Checks: []string{"rolling-maintenance-disabled"}},
	"schema-verify":        {Reobserve: true, Checks: []string{"schema-compatibility"}},
	"storage-verify":       {Kind: StorageVerification},
	"production-verify":    {Reobserve: true, Checks: []string{"database-health", "key-access", "overlay-connectivity", "image-access", "ingress-acknowledgements", "certificate-trust", "console-login"}},
	"recovery-verify":      {Reobserve: true, Kind: PointVerification, PointScope: SelectedPoint},
	"recovery-fence":       {Reobserve: true, Checks: []string{"provider-or-host-fence", "prior-authority-disabled"}},
	"recovery-database":    {Checks: []string{"empty-destination", "schemas-not-initialized"}},
	"restore":              {Admission: []RecoveryAdmission{FenceAdmission}, Checks: []string{"database-restored", "selected-release"}},
	"recovery-authority":   {Checks: []string{"new-ca-without-overlap", "client-identities", "console-sessions-invalidated", "registry-authority", "all-participants-paused", "host-admin-admission"}},
	"recovery-inventory":   {Kind: FleetVerification},
	"recovery-reserve":     {Checks: []string{"network-reservations"}},
	"recovery-reconcile":   {Admission: []RecoveryAdmission{ApprovalAdmission}, Checks: []string{"quarantine-preserved", "approved-desired-state"}},
	"recovery-checkpoints": {Admission: []RecoveryAdmission{ApprovalAdmission}, Checks: []string{"complete-checkpoints"}},
	"recovery-work":        {Admission: []RecoveryAdmission{ApprovalAdmission}, Checks: []string{"stale-build-ownership-invalidated", "new-worker-authority", "external-effects-reconciled"}},
	"recovery-resume":      {Activation: true, Admission: []RecoveryAdmission{AuthorityAdmission, FenceAdmission, RuntimeAdmission, ReportAdmission, ApprovalAdmission, PointAdmission, CheckpointAdmission}, LiveGates: []string{"production-verify", "recovery-checkpoints", "recovery-work", "backup"}, Checks: []string{"all-participants-resumed"}},
}

func ContractForHook(name string) (HookContract, bool) {
	c, ok := hookContracts[name]
	// Callers cannot modify the shared policy through a returned slice.
	c.Checks = append([]string(nil), c.Checks...)
	c.LiveGates = append([]string(nil), c.LiveGates...)
	c.Admission = append([]RecoveryAdmission(nil), c.Admission...)
	return c, ok
}

func RequiredHooks() []string {
	var names []string
	for name := range hookContracts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ValidateHookEvidence(name string, p Plan, e Evidence) error {
	c, ok := ContractForHook(name)
	if !ok {
		return fmt.Errorf("unknown verification contract %s", name)
	}
	switch c.Kind {
	case ReceiptVerification:
		if len(c.Checks) == 0 {
			return fmt.Errorf("receipt contract %s has no proof obligations", name)
		}
	case DatabaseVerification:
		if e.Database == nil || !e.Database.Replicated || len(e.Database.Members) == 0 || len(e.Database.Live) < len(e.Database.Members)/2+1 {
			return fmt.Errorf("database verification requires native membership, replication and live quorum")
		}
	case StorageVerification:
		for name := range p.Installation.Storage {
			if observed, ok := e.Storage[name]; !ok || !observed.Verified || len(observed.Hosts) == 0 {
				return fmt.Errorf("storage verification requires native inspection of %s", name)
			}
		}
	case PointVerification:
		return validateRecoveryEvidence(e, p.Installation.ID, p.Installation.Backup.Target, time.Now())
	case FleetVerification:
		if e.Fleet == nil {
			return fmt.Errorf("fleet verification requires actual surviving-host inventory")
		}
	default:
		return fmt.Errorf("unsupported verification kind for %s", name)
	}
	if len(c.Checks) > 0 {
		return validateReceipt(e, p, c.Checks...)
	}
	return nil
}

func validateHookState(name string, p Plan, state State, e Evidence, now time.Time) error {
	if err := ValidateHookEvidence(name, p, e); err != nil {
		return err
	}
	contract, _ := ContractForHook(name)
	switch contract.PointScope {
	case SelectedPoint:
		if state.LastRestore == nil || e.Backup != state.LastRestore.Backup || !e.DataLossCutoff.Equal(state.LastRestore.DataLossCutoff) {
			return fmt.Errorf("selected restore cutoff or protected point differs from native verification")
		}
		if e.Point.Snapshot.Schema != p.Release.Schema || e.Point.Snapshot.ConsoleSchema != p.Release.ConsoleSchema {
			return fmt.Errorf("selected point requires its corresponding release schemas")
		}
	case SuccessorPoint:
		if now.Sub(e.DataLossCutoff) > recovery.Objective {
			return fmt.Errorf("latest complete point exceeds the 15-minute objective")
		}
		if p.Recovery {
			if state.Recovery == nil || state.LastRestore == nil || e.DataLossCutoff.Before(state.Recovery.StartedAt) || !e.DataLossCutoff.After(state.LastRestore.DataLossCutoff) {
				return fmt.Errorf("recovery requires a new complete point after declaration and the selected cutoff")
			}
			if prior := state.Recovery.NewRecoveryPoint; prior != nil && e.DataLossCutoff.Before(prior.DataLossCutoff) {
				return fmt.Errorf("live complete point is older than the recorded recovery point")
			}
		}
	}
	return nil
}
