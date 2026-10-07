package productionops

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
	"github.com/gofrs/flock"
)

func Run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("operations", flag.ContinueOnError)
	planPath := fs.String("plan", os.Getenv("PLATFORM_PLAN"), "applied plan JSON")
	configPath := fs.String("config", os.Getenv("PLATFORM_OPERATIONS_CONFIG"), "private service selection JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		return fmt.Errorf("usage: operations [--plan FILE] [--config FILE] <lifecycle|verify lifecycle|ready role instance probe-file>")
	}
	if args[0] == "recover-files" {
		if len(args) != 4 {
			return fmt.Errorf("recover-files requires independent recovery config, pinned point URL, and destination")
		}
		files, err := RecoverFiles(ctx, args[1], args[2], args[3])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(files)
	}
	if args[0] == "offline-recovery" {
		if len(args) != 3 {
			return fmt.Errorf("offline-recovery requires workspace and native input")
		}
		ready, err := offlineRecovery(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(ready)
	}
	if args[0] == "sql-client" {
		if len(args) != 3 {
			return fmt.Errorf("sql-client requires private database URL and console schema")
		}
		return inspectSQLClient(ctx, args[1], args[2])
	}
	if args[0] == "process-admission" {
		if len(args) != 5 {
			return fmt.Errorf("process-admission requires unit, authority file, installation and generation")
		}
		return processAdmission(ctx, args[1], args[2], args[3], args[4])
	}
	if args[0] == "connect" {
		if len(args) != 2 {
			return fmt.Errorf("connect requires an address")
		}
		c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", args[1])
		if err != nil {
			return err
		}
		return c.Close()
	}
	if args[0] == "probe" {
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("probe requires a private probe file")
		}
		var p Probe
		if err := privateJSON(args[1], &p); err != nil {
			return err
		}
		b, err := probeHTTP(ctx, p, len(args) == 3 && args[2] == "unauthorized")
		if err != nil {
			return err
		}
		_, err = io.Copy(out, strings.NewReader(string(b)))
		return err
	}
	if args[0] == "ready" {
		if len(args) != 4 {
			return fmt.Errorf("ready requires role, instance and probe file")
		}
		return Ready(ctx, deploy.Role(args[1]), args[2], args[3])
	}
	var p deploy.Plan
	if err := privateJSON(*planPath, &p); err != nil {
		return err
	}
	if *configPath == "" {
		*configPath = p.Installation.OperationsConfig
	}
	var c Config
	if err := privateJSON(*configPath, &c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if p.ID == "" && len(args) == 3 && args[0] == "verify" && (args[1] == "database-verify" || args[1] == "storage-verify") {
		return fmt.Errorf("unexpected verification arguments")
	}
	if p.ID == "" && len(args) == 2 && args[0] == "verify" && (args[1] == "database-verify" || args[1] == "storage-verify") {
		if err := p.Installation.Validate(p.Release); err != nil {
			return err
		}
		if p.Generation == "" {
			return fmt.Errorf("inspection requires the applied generation")
		}
	} else if err := p.Validate(); err != nil {
		return err
	}
	r := Runner{Plan: p, Config: c}
	r.defaults()
	if path := os.Getenv("PLATFORM_RECOVERY_OPERATION"); path != "" {
		var progress deploy.RecoveryProgress
		if err := privateJSON(path, &progress); err != nil {
			return err
		}
		r.RecoveryProgress = &progress
	}
	if err := os.MkdirAll(c.StateDirectory, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(c.StateDirectory, "operations.lock"))
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("operations lock unavailable")
	}
	defer lock.Unlock()
	verify := args[0] == "verify"
	if verify {
		args = args[1:]
		if len(args) == 0 {
			return fmt.Errorf("verify requires lifecycle")
		}
	}
	if !verify {
		if err := r.Execute(ctx, args); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
	}
	result, err := r.Verify(ctx, args)
	if err != nil {
		if !verify && (args[0] == "resume" || args[0] == "recovery-resume") {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if pauseErr := r.pause(pauseCtx, true, args[0] == "recovery-resume"); pauseErr != nil {
				return fmt.Errorf("verify %s: %w; pause unresolved: %v", args[0], err, pauseErr)
			}
		}
		return fmt.Errorf("verify %s: %w", args[0], err)
	}
	return json.NewEncoder(out).Encode(result)
}

func (r *Runner) Execute(ctx context.Context, args []string) error {
	r.defaults()
	switch args[0] {
	case "credential-renew":
		return r.renewCredentials(ctx)
	case "database-credentials":
		return r.databaseCredentials(ctx, false)
	case "database-init":
		return r.initializeDatabase(ctx, false)
	case "platform-bootstrap":
		return r.bootstrap(ctx)
	case "credentials":
		return r.credentials(ctx, false)
	case "reservations":
		return r.reservations(ctx, false)
	case "quiesce":
		return r.pause(ctx, true, r.Plan.Recovery && r.RecoveryProgress != nil && r.RecoveryProgress.ApprovedDigest != "")
	case "resume":
		return r.resume(ctx, false)
	case "recovery-protect":
		return r.protect(ctx, false)
	case "backup-schedule":
		return r.backupSchedule(ctx, false)
	case "backup":
		return r.backup(ctx)
	case "backup-complete":
		if err := r.protect(ctx, false); err != nil {
			return err
		}
		return r.completeBackup(ctx)
	case "recovery-finalize":
		return r.finalize(ctx, false)
	case "database-verify", "storage-verify", "production-verify", "recovery-verify":
		return nil
	case "recovery-fence":
		return r.fence(ctx, false)
	case "recovery-database":
		return r.initializeDatabase(ctx, true)
	case "restore":
		return r.restore(ctx)
	case "recovery-authority":
		return r.authority(ctx, false)
	case "recovery-inventory":
		return r.captureFleet(ctx)
	case "recovery-reserve":
		return r.reserveFleet(ctx, false)
	case "recovery-reconcile":
		return r.reconcileFleet(ctx, false)
	case "recovery-checkpoints":
		return r.checkpoints(ctx, false)
	case "recovery-work":
		return r.recoverWork(ctx, false)
	case "recovery-resume":
		return r.resume(ctx, true)
	case "drain", "retire":
		return r.lifecycle(ctx, args, false)
	case "verify-drain", "verify-retirement":
		return nil
	default:
		return fmt.Errorf("unknown lifecycle %s", args[0])
	}
}
func (r *Runner) Verify(ctx context.Context, args []string) (any, error) {
	r.defaults()
	e := deploy.Evidence{Recovery: &deploy.RecoveryReceipt{Installation: r.Plan.Installation.ID, Generation: r.Plan.Generation, Checks: map[string]bool{}}}
	check := func(names ...string) {
		for _, name := range names {
			e.Recovery.Checks[name] = true
		}
	}
	var err error
	switch args[0] {
	case "credential-renew":
		err = r.credentials(ctx, true)
	case "database-credentials":
		err = r.databaseCredentials(ctx, true)
	case "database-init":
		err = r.verifyFreshDatabaseReady(ctx)
	case "platform-bootstrap":
		err = r.verifyBootstrap(ctx)
	case "credentials":
		err = r.credentials(ctx, true)
	case "reservations":
		err = r.reservations(ctx, true)
	case "quiesce":
		err = r.verifyPause(ctx, true, false)
	case "resume":
		err = r.verifyPause(ctx, false, false)
	case "recovery-protect":
		err = r.protect(ctx, true)
	case "backup-schedule":
		err = r.backupSchedule(ctx, true)
	case "backup", "backup-complete":
		return r.backupEvidence(ctx)
	case "recovery-finalize":
		err = r.finalize(ctx, true)
	case "database-verify":
		return r.databaseStatus(ctx)
	case "storage-verify":
		return r.storageStatus(ctx)
	case "production-verify":
		err = r.productionVerify(ctx)
		if err == nil {
			check("database-health", "key-access", "overlay-connectivity", "image-access", "ingress-acknowledgements", "certificate-trust", "console-login")
		}
	case "recovery-verify":
		return r.selectedPoint(ctx)
	case "recovery-fence":
		err = r.fence(ctx, true)
		if err == nil {
			check("provider-or-host-fence", "prior-authority-disabled")
		}
	case "recovery-database":
		err = r.verifyEmptyDatabase(ctx)
		if err == nil {
			check("empty-destination", "schemas-not-initialized")
		}
	case "restore":
		err = r.verifyRestore(ctx)
		if err == nil {
			check("database-restored", "selected-release")
		}
	case "recovery-authority":
		err = r.authority(ctx, true)
		if err == nil {
			check("new-ca-without-overlap", "client-identities", "console-sessions-invalidated", "registry-authority", "all-participants-paused", "host-admin-admission")
		}
	case "recovery-inventory":
		e.Fleet, err = r.readFleet(ctx)
	case "recovery-reserve":
		err = r.reserveFleet(ctx, true)
		if err == nil {
			check("network-reservations")
		}
	case "recovery-reconcile":
		err = r.reconcileFleet(ctx, true)
		if err == nil {
			check("quarantine-preserved", "approved-desired-state")
		}
	case "recovery-checkpoints":
		e.Recovery.Acknowledgements, err = r.verifyCheckpoints(ctx)
		if err == nil {
			check("complete-checkpoints")
		}
	case "recovery-work":
		err = r.recoverWork(ctx, true)
		if err == nil {
			check("stale-build-ownership-invalidated", "new-worker-leases", "external-effects-reconciled")
		}
	case "recovery-resume":
		err = r.verifyPause(ctx, false, true)
		if err == nil {
			check("all-participants-resumed")
		}
	case "drain", "retire", "verify-drain", "verify-retirement":
		err = r.lifecycle(ctx, args, true)
	default:
		return nil, fmt.Errorf("unknown lifecycle %s", args[0])
	}
	if err != nil {
		return nil, err
	}
	if r.RecoveryProgress != nil && r.RecoveryProgress.Report != nil {
		e.Recovery.ReportDigest = r.RecoveryProgress.Report.ApprovalDigest()
	}
	return e, nil
}
