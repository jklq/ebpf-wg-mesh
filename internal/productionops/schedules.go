package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/deploy"
)

func (r *Runner) completionHosts() ([]string, error) {
	hosts := append([]string{}, r.Config.CompletionHosts...)
	if len(hosts) == 0 {
		for _, pl := range r.Plan.Placements {
			if pl.Role == deploy.ControlPlane && !slices.Contains(hosts, pl.Host) {
				hosts = append(hosts, pl.Host)
			}
		}
	}
	if len(hosts) == 0 {
		hosts = []string{r.Plan.AdministrationHost}
	}
	if r.Plan.Installation.OneHostFailure && len(hosts) < 2 {
		return nil, fmt.Errorf("redundant recovery completion needs at least two independent hosts")
	}
	domains := map[string]bool{}
	for _, id := range hosts {
		h, ok := r.Plan.Installation.Host(id)
		if !ok || !h.Trusted || h.Reliability != "reliable" {
			return nil, fmt.Errorf("backup completion host must be trusted, reliable and in the applied inventory")
		}
		domains[h.FailureDomain] = true
	}
	if r.Plan.Installation.OneHostFailure && len(domains) < 2 {
		return nil, fmt.Errorf("backup completers share a failure domain")
	}
	return hosts, nil
}

func (r *Runner) completionTimer(ctx context.Context, verify bool) error {
	hosts, err := r.completionHosts()
	if err != nil {
		return err
	}
	// Protection snapshots are local, mutable output of each completer. They
	// must not be distributed or frozen as schedule inputs: pre-cutover backup
	// and independently running completers may regenerate different closures.
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	inputs, err := r.externalInputs(c)
	if err != nil {
		return err
	}
	inputs[c.RecoveryKeyFile] = s.RecoveryKey
	for _, path := range []string{c.KeyringFile, r.Config.Console.TokenKeyFile, r.Config.Database.URLFile} {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inputs[path] = b
	}
	// A completer is a host administrator, never a platform login. The explicit
	// trusted-host selection governs where its independent keys may be provisioned.
	err = filepath.WalkDir(r.databasePKI(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil {
			inputs[path] = b
		}
		return err
	})
	if err != nil {
		return err
	}
	scheduleDirectory := r.completionDirectory()
	planPath := filepath.Join(scheduleDirectory, "plan.json")
	stateSource := "/etc/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/installer-state.json"
	statePath := filepath.Join(scheduleDirectory, "installer-state.json")
	if data, err := os.ReadFile(stateSource); err == nil {
		var state deploy.State
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
		// Attempt progress changes after every effect. The independent service needs
		// the actual host bindings and resource inventory, without that mutable journal.
		snapshot := deploy.State{Version: 1, InstallationID: r.Plan.Installation.ID, Generation: r.Plan.Generation, Policy: &r.Plan.Installation, Bundle: &r.Plan.Release, Placements: r.Plan.Placements, Retained: state.Retained, Bindings: state.Bindings}
		data, err = json.Marshal(snapshot)
		if err != nil {
			return err
		}
		inputs[statePath] = data
	} else if !os.IsNotExist(err) {
		return err
	}
	inputs[planPath], err = json.Marshal(r.Plan)
	if err != nil {
		return err
	}
	for _, id := range hosts {
		name := "platform-" + r.Plan.Installation.ID + "-recovery"
		config := filepath.Join(scheduleDirectory, "operations.json")
		baseConfig := filepath.Join(scheduleDirectory, "base-recovery.json")
		if r.Plan.Installation.OperationsConfig == "" {
			return fmt.Errorf("reference schedules require operationsConfig")
		}
		units := r.completionUnits()
		var script strings.Builder
		// Native executables are already staged by the applied plan. Rewrite the
		// selected config to those pinned executables on every independent completer.
		selected := r.Config
		selected.RecoveryConfig = baseConfig
		selected.Database.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/cockroachdb"
		selected.Console.AdminBinary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/console-admin"
		files := map[string][]byte{}
		for path, b := range inputs {
			files[path] = b
		}
		files[config], _ = json.Marshal(selected)
		base, baseService, err := recoveryConfig(r.Config.RecoveryConfig)
		if err != nil {
			return err
		}
		clear(baseService.RecoveryKey)
		base.Images.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/skopeo"
		// Service-specific paths live in generated files. Operator inputs keep
		// their exact original bytes, including harmless JSON formatting.
		files[baseConfig], _ = json.Marshal(base)
		for _, path := range sortedFiles(files) {
			if verify {
				// Fleet inventory is refreshed by each completed backup; its content is
				// inspected and replicated by that operation, rather than frozen here.
				if path == r.Plan.Installation.Recovery.Inventory {
					continue
				}
				script.WriteString(verifyRemoteFile(path, files[path]))
			} else {
				script.WriteString(remoteFile(path, files[path]))
			}
		}
		for filename, data := range units {
			path := "/etc/systemd/system/" + filename
			if verify {
				script.WriteString(verifyRemoteFile(path, []byte(data)))
			} else {
				script.WriteString(remoteFile(path, []byte(data)))
			}
		}
		if !verify {
			script.WriteString("systemctl daemon-reload\nsystemctl enable --now " + shell(name+".timer") + " " + shell(name+"-credentials.timer") + "\n")
		}
		script.WriteString("systemctl is-active --quiet " + shell(name+".timer") + "\nsystemctl is-active --quiet " + shell(name+"-credentials.timer") + "\n")
		if _, err = r.remote(ctx, r.Plan, deploy.Placement{Host: id}, script.String()); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) completionDirectory() string {
	return filepath.Join(r.Config.StateDirectory, r.Plan.ID, "schedule")
}

func (r *Runner) completionUnits() map[string]string {
	name := "platform-" + r.Plan.Installation.ID + "-recovery"
	binary := operationsBinary(r.Plan)
	directory := r.completionDirectory()
	planPath, statePath := filepath.Join(directory, "plan.json"), filepath.Join(directory, "installer-state.json")
	config := filepath.Join(directory, "operations.json")
	service := "[Unit]\nDescription=Complete independent platform recovery point\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=12min\nEnvironment=PLATFORM_DEPLOYMENT_STATE=" + statePath + "\nExecStart=" + binary + " --plan " + planPath + " --config " + config + " backup-complete\n"
	timer := "[Unit]\nDescription=Independent platform recovery completion\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=1min\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	renewal := "[Unit]\nDescription=Renew native platform credentials\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=10min\nExecStart=" + binary + " --plan " + planPath + " --config " + config + " credential-renew\n"
	renewalTimer := "[Timer]\nOnBootSec=10min\nOnUnitActiveSec=1h\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	units := map[string]string{name + ".service": service, name + ".timer": timer, name + "-credentials.service": renewal, name + "-credentials.timer": renewalTimer}
	return units
}

// Stop queued old maintenance jobs before replacing their units. Plan-specific
// immutable inputs prevent a queued process from pairing new inventory with an
// old executable or service configuration.
func (r *Runner) stopPriorMaintenance(ctx context.Context) error {
	return r.priorMaintenance(ctx, true)
}

func (r *Runner) priorMaintenance(ctx context.Context, stop bool) error {
	if r.Plan.Previous == nil || r.Plan.Recovery {
		return nil
	}
	currentHosts, err := r.completionHosts()
	if err != nil {
		return err
	}
	currentUnits := r.completionUnits()
	old := r.Plan.Previous
	prior := deploy.Plan{Installation: old.Installation, Release: old.Release, Placements: old.Placements, Previous: old, Generation: r.Plan.Generation}
	for _, host := range prior.Installation.Hosts {
		if r.Plan.Automatic {
			if _, err := r.remote(ctx, prior, deploy.Placement{Host: host.ID}, "true\n"); err != nil {
				// Presence does not establish fencing. SQL admission prevents a
				// returning old scheduled process from acquiring current ownership.
				continue
			}
		}
		name := "platform-" + prior.Installation.ID + "-recovery"
		var script strings.Builder
		for _, service := range []string{name, name + "-credentials"} {
			timer, job := shell(service+".timer"), shell(service+".service")
			// A retry after partial resume pauses the current authority while
			// retaining the already verified replacement schedules.
			current := slices.Contains(currentHosts, host.ID)
			if current {
				script.WriteString("if (\n" + verifyRemoteFile("/etc/systemd/system/"+service+".timer", []byte(currentUnits[service+".timer"])) + verifyRemoteFile("/etc/systemd/system/"+service+".service", []byte(currentUnits[service+".service"])) + ") 2>/dev/null; then :; else\n")
			}
			script.WriteString("case \"$(systemctl show --value -p LoadState " + timer + ")\" in\nnot-found) ;;\nloaded)\n")
			if stop {
				script.WriteString("systemctl disable --now " + timer + "\nsystemctl stop " + job + "\n")
			}
			script.WriteString("test \"$(systemctl show --value -p ActiveState " + timer + ")\" = inactive\ncase \"$(systemctl show --value -p ActiveState " + job + ")\" in inactive|failed) ;; *) exit 1;; esac\ntest \"$(systemctl show --value -p MainPID " + job + ")\" = 0\ncase \"$(systemctl is-enabled " + timer + " 2>/dev/null || true)\" in disabled|masked) ;; *) exit 1;; esac\n;;\n*) exit 1;;\nesac\n")
			if current {
				script.WriteString("fi\n")
			}
		}
		if _, err := r.remote(ctx, prior, deploy.Placement{Host: host.ID}, script.String()); err != nil {
			return fmt.Errorf("prior maintenance shutdown is unresolved: %w", err)
		}
	}
	return nil
}

func (r *Runner) renewCredentials(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	release, err := r.acquireRenewal(ctx, db)
	if err != nil {
		return err
	}
	defer release()
	// Presence permits renewing an existing managed process; it does not permit
	// database membership changes, unmasking or resurrection of retired units.
	current := *r
	current.Plan = r.Plan
	current.Plan.Automatic = false
	current.Plan.Placements = nil
	var offline []string
	checked := map[string]bool{}
	for _, pl := range r.Plan.Placements {
		online, known := checked[pl.Host]
		if !known {
			_, err := r.remote(ctx, r.Plan, pl, "true\n")
			online = err == nil
			checked[pl.Host] = online
			if !online {
				offline = append(offline, pl.Host)
			}
		}
		if online {
			current.Plan.Placements = append(current.Plan.Placements, pl)
		}
	}
	if err = current.databaseCredentials(ctx, false); err != nil {
		return err
	}
	if err = current.credentials(ctx, false); err != nil {
		return err
	}
	for _, pl := range current.Plan.Placements {
		if err = current.activateCredentials(ctx, pl); err != nil {
			return err
		}
	}
	if err = current.credentials(ctx, true); err != nil {
		return err
	}
	if len(offline) > 0 {
		return fmt.Errorf("online credentials renewed; unavailable hosts require later inspection: %v", offline)
	}
	return nil
}
