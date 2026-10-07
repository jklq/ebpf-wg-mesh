package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/recovery"
)

// Host-admin helpers consumed by the pinned release tool. They require operator
// database credentials and remain independent of console login and platform RPC.
func runFleetRecovery(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("platformctl recovery "+args[0], flag.ContinueOnError)
	var urlFile, fleetFile, reportFile, installation, generation, keyring, progressFile, planFile string
	fs.StringVar(&urlFile, "database-url-file", "", "private operator database URL file")
	fs.StringVar(&fleetFile, "fleet", "", "latest external FleetInput JSON; desired state is read from restored SQL")
	fs.StringVar(&reportFile, "report", "", "approved FleetReport JSON")
	fs.StringVar(&installation, "installation", os.Getenv("PLATFORM_INSTALLATION"), "stable installation identity")
	fs.StringVar(&generation, "generation", os.Getenv("PLATFORM_RECOVERY_GENERATION"), "persisted recovery generation")
	fs.StringVar(&keyring, "keyring", "", "recovered envelope master keyring")
	fs.StringVar(&progressFile, "progress", os.Getenv("PLATFORM_RECOVERY_OPERATION"), "independently recorded and verified RecoveryProgress JSON")
	fs.StringVar(&planFile, "plan", os.Getenv("PLATFORM_PLAN"), "the approved restore plan JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || installation == "" || generation == "" {
		return fmt.Errorf("recovery requires installation and generation")
	}
	url, err := readSecretFile("operator-database", urlFile)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", strings.TrimSpace(string(url)))
	if err != nil {
		return err
	}
	defer db.Close()
	e := Evidence{Recovery: &RecoveryReceipt{Installation: installation, Generation: generation, Checks: map[string]bool{}}}
	switch args[0] {
	case "empty-destination":
		if err := recovery.CheckEmptyDestination(ctx, db); err != nil {
			return err
		}
		e.Recovery.Checks["empty-destination"], e.Recovery.Checks["schemas-not-initialized"] = true, true
	case "fleet-inventory":
		b, err := os.ReadFile(fleetFile)
		if err != nil {
			return err
		}
		var input recovery.FleetInput
		if err := json.Unmarshal(b, &input); err != nil {
			return err
		}
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
		e.Fleet = &input
	case "reserve-fleet":
		b, err := os.ReadFile(reportFile)
		if err != nil {
			return err
		}
		var report recovery.FleetReport
		if err := json.Unmarshal(b, &report); err != nil {
			return err
		}
		if report.Installation != installation || report.Generation != generation {
			return fmt.Errorf("report differs from recovery operation")
		}
		if err := recovery.ReserveFleet(ctx, db, report); err != nil {
			return err
		}
		e.Recovery.ReportDigest = report.ApprovalDigest()
		e.Recovery.Checks["network-reservations"] = true
	case "reset-authority":
		keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{})
		if err != nil {
			return err
		}
		defer keys.Provider().Close()
		if err := signkeys.New(db, keys.Registry()).ResetForRecovery(ctx, installation, generation); err != nil {
			return err
		}
		e.Recovery.Checks["new-ca-without-overlap"], e.Recovery.Checks["registry-authority"] = true, true
	case "initialize-authority":
		var oldInstallation, oldGeneration string
		if _, err := db.ExecContext(ctx, `INSERT INTO recovery_runtime_authority(singleton,installation,generation,paused) VALUES (TRUE,$1,$2,FALSE) ON CONFLICT DO NOTHING`, installation, generation); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT installation,generation FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&oldInstallation, &oldGeneration); err != nil {
			return err
		}
		if oldInstallation != installation || oldGeneration != generation {
			return fmt.Errorf("initial authority already belongs to a different generation")
		}
	case "resume-authority":
		progress, err := Load[RecoveryProgress](progressFile)
		if err != nil {
			return err
		}
		plan, err := Load[Plan](planFile)
		if err != nil {
			return err
		}
		if err := plan.Validate(); err != nil {
			return err
		}
		if plan.Installation.ID != installation || plan.Generation != generation {
			return fmt.Errorf("resume authority differs from the recovery plan")
		}
		if err := validateRecoveryResume(plan, &progress, time.Now()); err != nil {
			return err
		}
		result, err := db.ExecContext(ctx, `UPDATE recovery_runtime_authority SET paused=FALSE WHERE singleton=TRUE AND installation=$1 AND generation=$2`, installation, generation)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("resume authority differs from database generation")
		}
		e.Recovery.Checks["shared-authority-resumed"] = true
	default:
		return fmt.Errorf("unknown host-admin recovery command")
	}
	return json.NewEncoder(out).Encode(e)
}
