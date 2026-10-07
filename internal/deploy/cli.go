package deploy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func Run(ctx context.Context, args []string, out io.Writer, driver Driver) error {
	if len(args) > 0 && args[0] == "database-status" {
		return RunDatabaseStatus(ctx, args[1:], out)
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: platformctl <init-state|plan|apply|reconcile|status|restore> [flags]")
	}
	fs := flag.NewFlagSet("platformctl "+args[0], flag.ContinueOnError)
	var manifest, bundle, output, statePath, keyPath string
	var watch bool
	var interval time.Duration
	var backup, cutoff string
	fs.StringVar(&statePath, "state", envDefault("PLATFORMCTL_STATE", "/var/lib/ebpf-wg-mesh/deployment/state.enc"), "encrypted state outside the platform database")
	fs.StringVar(&keyPath, "key-file", os.Getenv("PLATFORMCTL_KEY_FILE"), "private file containing 32 raw key bytes; retain with recovery credentials")
	fs.StringVar(&manifest, "installation", "", "typed installation YAML")
	fs.StringVar(&bundle, "bundle", "", "release bundle (defaults to releases/<release>.yaml beside the installation)")
	fs.StringVar(&output, "output", "", "write plan JSON to this file")
	fs.BoolVar(&watch, "watch", false, "continuously reconcile the applied policy")
	fs.DurationVar(&interval, "interval", 30*time.Second, "reconciliation interval")
	fs.StringVar(&backup, "backup", "", "verified complete backup to restore")
	fs.StringVar(&cutoff, "data-loss-cutoff", "", "backup's mutation cutoff as RFC3339")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 1 || (args[0] != "apply" && fs.NArg() != 0) {
		return fmt.Errorf("unexpected arguments")
	}
	if keyPath == "" || !filepath.IsAbs(keyPath) {
		return fmt.Errorf("--key-file or PLATFORMCTL_KEY_FILE must be an absolute private key-file path")
	}
	if args[0] == "init-state" {
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			return err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		f, err := os.OpenFile(keyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state key must be a private regular file")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	store, err := OpenState(statePath, key)
	if err != nil {
		return err
	}
	defer store.Close()
	engine := Engine{Store: store, Driver: driver}
	if args[0] == "resume" {
		state, err := store.Read()
		if err != nil {
			return err
		}
		if state.Progress == nil || state.Progress.Plan == nil {
			return fmt.Errorf("no interrupted plan to resume")
		}
		for _, op := range state.Progress.Plan.Operations {
			if op.Hook == "restore" && state.LastRestore != nil {
				fmt.Fprintf(out, "resuming restore %s; data-loss cutoff %s\n", state.LastRestore.Backup, state.LastRestore.DataLossCutoff.Format(time.RFC3339))
				break
			}
		}
		return engine.Apply(ctx, *state.Progress.Plan)
	}
	if args[0] == "apply" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: platformctl apply [--state PATH --key-file PATH] plan.json")
		}
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, 16<<20))
		decoder.DisallowUnknownFields()
		var p Plan
		if err := decoder.Decode(&p); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("plan must contain one JSON document")
		}
		return engine.Apply(ctx, p)
	}
	if args[0] == "status" {
		state, err := store.Read()
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(state)
	}
	if !contains([]string{"plan", "reconcile", "restore"}, args[0]) {
		return fmt.Errorf("unknown command %s", args[0])
	}
	if manifest == "" {
		return fmt.Errorf("--installation is required")
	}
	i, err := Load[Installation](manifest)
	if err != nil {
		return err
	}
	if bundle == "" {
		bundle = filepath.Join(filepath.Dir(manifest), "releases", i.Release+".yaml")
	}
	r, err := Load[Release](bundle)
	if err != nil {
		return err
	}
	if err := i.Validate(r); err != nil {
		return err
	}
	if args[0] == "restore" {
		t, err := time.Parse(time.RFC3339, cutoff)
		if err != nil || backup == "" {
			return fmt.Errorf("restore requires --backup and its --data-loss-cutoff; all mutations after that time will be lost")
		}
		state, err := store.Read()
		if err != nil {
			return err
		}
		if state.InstallationID != "" && state.InstallationID != i.ID {
			return fmt.Errorf("restore must preserve installation identity %s", state.InstallationID)
		}
		if state.Progress != nil && state.Progress.Plan != nil {
			for _, op := range state.Progress.Plan.Operations {
				_, completed := state.Progress.Completed[op.ID]
				if op.Kind == "purchase" && state.Progress.Started[op.ID] && !completed && state.Bindings[op.Host].ServerID == "" {
					return fmt.Errorf("unknown purchase outcome for %s; resume provider discovery before restoration", op.Host)
				}
			}
		}
		inv, err := driver.Inventory(ctx, i, r, state)
		if err != nil {
			return err
		}
		p, err := BuildPlan(i, r, State{Version: 1, Bindings: state.Bindings}, inv, false, time.Now())
		if err != nil {
			return err
		}
		// Restore is an explicitly selected flat cutover. Independent fencing
		// precedes stopping the prior release; the pinned restore hook
		// must validate completeness and materialize the target schema/keyring.
		p.StateRevision = state.Revision
		p.StateDigest = Digest(state)
		p.Previous = previousDeployment(state)
		var ops []Operation
		add := func(kind, host, hook string, pl *Placement) {
			op := Operation{Kind: kind, Host: host, Hook: hook, Placement: pl}
			op.ID = Digest([]any{op, len(ops)})[:24]
			ops = append(ops, op)
		}
		for _, op := range p.Operations {
			if op.Kind == "stage" || op.Kind == "stage-tools" || op.Kind == "purchase" {
				ops = append(ops, op)
			}
		}
		// Recovery must work after permanent host loss. Independent provider or
		// gateway fencing proves that old instances cannot mutate or serve traffic;
		// a missing original machine cannot be required to answer SSH.
		add("hook", p.AdministrationHost, "recovery-fence", nil)
		for _, pl := range state.Placements {
			if _, present := i.Host(pl.Host); present {
				add("stop", pl.Host, "", &pl)
			}
		}
		add("hook", p.AdministrationHost, "restore", nil)
		for _, op := range p.Operations {
			if op.Kind != "stage" && op.Kind != "stage-tools" && op.Kind != "purchase" && op.Hook != "database-init" && op.Hook != "platform-bootstrap" {
				ops = append(ops, op)
			}
		}
		add("hook", p.AdministrationHost, "resume", nil)
		p.Operations = ops
		if len(p.Unmet) > 0 {
			return fmt.Errorf("recovery inventory has unmet placement requirements: %v", p.Unmet)
		}
		// The cutoff is recorded before any destructive restoration and is
		// visible in status, even if the restoration is interrupted.
		restoration := Evidence{Backup: backup, DataLossCutoff: t}
		state.LastBackup = restoration
		state.LastRestore = &restoration
		state.Progress = nil
		state.InstallationID = i.ID
		p.StateDigest = Digest(state)
		p.ID = p.digest()
		state.Progress = &Progress{PlanID: p.ID, Plan: &p, Started: map[string]bool{}, Completed: map[string]Evidence{}}
		if err := store.Write(state); err != nil {
			return err
		}
		fmt.Fprintf(out, "restoring %s; data-loss cutoff %s\n", backup, t.Format(time.RFC3339))
		return engine.Apply(ctx, p)
	}
	if watch && (args[0] != "reconcile" || interval < time.Second) {
		return fmt.Errorf("--watch requires reconcile and an interval of at least one second")
	}
	for {
		state, err := store.Read()
		if err != nil {
			return err
		}
		if args[0] == "reconcile" && state.Progress != nil {
			if state.Progress.Plan == nil || !state.Progress.Plan.Automatic {
				return fmt.Errorf("resume the interrupted applied plan with platformctl resume")
			}
			if Digest(state.Progress.Plan.Installation) != Digest(i) || Digest(state.Progress.Plan.Release) != Digest(r) {
				return fmt.Errorf("interrupted reconciliation belongs to a different policy")
			}
			if err := engine.Apply(ctx, *state.Progress.Plan); err != nil {
				if !watch {
					return err
				}
				fmt.Fprintf(out, "reconciliation pending: %v\n", err)
				if err := waitReconcile(ctx, interval); err != nil {
					return err
				}
				continue
			}
			state, err = store.Read()
			if err != nil {
				return err
			}
		}
		inv, err := driver.Inventory(ctx, i, r, state)
		if err != nil {
			return err
		}
		p, err := BuildPlan(i, r, state, inv, args[0] == "reconcile", time.Now())
		if err != nil {
			return err
		}
		if args[0] == "plan" {
			raw, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			if output != "" {
				return WritePrivate(output, raw)
			}
			_, err = out.Write(raw)
			return err
		}
		if err := engine.Apply(ctx, p); err != nil {
			if !watch {
				return err
			}
			fmt.Fprintf(out, "reconciliation pending: %v\n", err)
		}
		if !watch {
			return nil
		}
		fmt.Fprintf(out, "reconciled installation %s\n", i.ID)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func waitReconcile(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
