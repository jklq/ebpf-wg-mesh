package productionops

import (
	"context"
	"fmt"
)

// Native proof bindings are independent of the required contract. A new
// requirement must have an implemented inspection before it can be claimed.
// Related proofs share an inspection, executed once in each verification call.
var nativeProofInspections = map[string]string{
	"database-tls-provisioned":          "database-credentials",
	"schema-compatibility":              "schema-verify",
	"database-version-finalized":        "database-finalize",
	"builder-work-drained":              "builder-drain",
	"builder-drain-restored":            "builder-resume",
	"rolling-maintenance-disabled":      "rolling-prepare",
	"fresh-database-admitted":           "database-init",
	"platform-bootstrap-ready":          "platform-bootstrap",
	"component-identities-provisioned":  "credentials",
	"scheduler-reservations-admitted":   "reservations",
	"all-participants-paused":           "quiesce",
	"prior-maintenance-disabled":        "quiesce",
	"all-participants-resumed":          "resume",
	"complete-dependencies-protected":   "recovery-protect",
	"independent-schedules-admitted":    "backup-schedule",
	"applied-release-finalized":         "recovery-finalize",
	"database-health":                   "production-verify",
	"key-access":                        "production-verify",
	"overlay-connectivity":              "production-verify",
	"image-access":                      "production-verify",
	"ingress-acknowledgements":          "production-verify",
	"certificate-trust":                 "production-verify",
	"console-login":                     "production-verify",
	"provider-or-host-fence":            "recovery-fence",
	"prior-authority-disabled":          "recovery-fence",
	"empty-destination":                 "recovery-database",
	"schemas-not-initialized":           "recovery-database",
	"database-restored":                 "restore",
	"selected-release":                  "restore",
	"new-ca-without-overlap":            "recovery-authority",
	"client-identities":                 "recovery-authority",
	"console-sessions-invalidated":      "recovery-authority",
	"registry-authority":                "recovery-authority",
	"host-admin-admission":              "recovery-authority",
	"network-reservations":              "recovery-reserve",
	"quarantine-preserved":              "recovery-reconcile",
	"approved-desired-state":            "recovery-reconcile",
	"complete-checkpoints":              "recovery-checkpoints",
	"stale-build-ownership-invalidated": "recovery-work",
	"new-worker-authority":              "recovery-work",
	"external-effects-reconciled":       "recovery-work",
}

var nativeProofOverrides = map[string]map[string]string{
	"recovery-resume":    {"all-participants-resumed": "recovery-resume"},
	"recovery-authority": {"all-participants-paused": "recovery-authority"},
}

func proofInspection(name, hook string) (string, error) {
	inspection, ok := nativeProofInspections[name]
	if !ok {
		return "", fmt.Errorf("required native proof %s has no implemented inspection", name)
	}
	// Both transitions inspect the same runtime pause/resume effect. Recovery
	// additionally checks its independently recorded authority and report.
	if override := nativeProofOverrides[hook][name]; override != "" {
		return override, nil
	}
	return inspection, nil
}

func (r *Runner) inspectProof(ctx context.Context, inspection string) error {
	switch inspection {
	case "database-credentials":
		return r.databaseCredentials(ctx, true)
	case "database-init":
		return r.verifyFreshDatabaseReady(ctx)
	case "schema-verify":
		return r.verifySchemaCompatibility(ctx)
	case "database-finalize":
		return r.finalizeDatabaseUpgrade(ctx, true)
	case "builder-drain", "builder-resume":
		return r.inspectBuilderUpgrade(ctx, inspection)
	case "rolling-prepare":
		return r.priorMaintenance(ctx, false)
	case "platform-bootstrap":
		return r.verifyBootstrap(ctx)
	case "credentials":
		return r.credentials(ctx, true)
	case "reservations":
		return r.reservations(ctx, true)
	case "quiesce":
		if err := r.verifyPause(ctx, true, false); err != nil {
			return err
		}
		return r.priorMaintenance(ctx, false)
	case "resume":
		return r.verifyPause(ctx, false, false)
	case "recovery-protect":
		return r.protect(ctx, true)
	case "backup-schedule":
		return r.backupSchedule(ctx, true)
	case "recovery-finalize":
		return r.finalize(ctx, true)
	case "production-verify":
		return r.productionVerify(ctx)
	case "recovery-fence":
		return r.fence(ctx, true)
	case "recovery-database":
		return r.verifyEmptyDatabase(ctx)
	case "restore":
		return r.verifyRestore(ctx)
	case "recovery-authority":
		return r.authority(ctx, true)
	case "recovery-reserve":
		return r.reserveFleet(ctx, true)
	case "recovery-reconcile":
		return r.reconcileFleet(ctx, true)
	case "recovery-checkpoints":
		_, err := r.verifyCheckpoints(ctx)
		return err
	case "recovery-work":
		return r.recoverWork(ctx, true)
	case "recovery-resume":
		return r.verifyPause(ctx, false, true)
	default:
		return fmt.Errorf("unknown native inspection %s", inspection)
	}
}
