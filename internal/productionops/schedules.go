package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	c, s, err := recoveryConfig(r.effectiveConfig())
	if err != nil {
		return err
	}
	defer clear(s.RecoveryKey)
	inputs, err := r.externalInputs(c)
	if err != nil {
		return err
	}
	inputs[c.RecoveryKeyFile] = s.RecoveryKey
	for _, path := range []string{c.KeyringFile, r.Config.Console.TokenKeyFile, r.Config.Database.URLFile, r.effectiveConfig()} {
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
	planPath := filepath.Join(r.Config.StateDirectory, "scheduled-plan.json")
	statePath := "/etc/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/installer-state.json"
	if data, err := os.ReadFile(statePath); err == nil {
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
		binary := operationsBinary(r.Plan)
		config := r.Plan.Installation.OperationsConfig
		if config == "" {
			return fmt.Errorf("reference schedules require operationsConfig")
		}
		service := "[Unit]\nDescription=Complete independent platform recovery point\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=12min\nEnvironment=PLATFORM_DEPLOYMENT_STATE=/etc/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/installer-state.json\nExecStart=" + binary + " --plan " + planPath + " --config " + config + " backup-complete\n"
		timer := "[Unit]\nDescription=Independent platform recovery completion\n[Timer]\nOnBootSec=1min\nOnUnitActiveSec=1min\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
		renewal := "[Unit]\nDescription=Renew native platform credentials\nAfter=network-online.target\n[Service]\nType=oneshot\nUMask=0077\nTimeoutStartSec=10min\nExecStart=" + binary + " --plan " + planPath + " --config " + config + " credential-renew\n"
		renewalTimer := "[Timer]\nOnBootSec=10min\nOnUnitActiveSec=1h\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
		units := map[string]string{name + ".service": service, name + ".timer": timer, name + "-credentials.service": renewal, name + "-credentials.timer": renewalTimer}
		var script strings.Builder
		// Native executables are already staged by the applied plan. Rewrite the
		// selected config to those pinned executables on every independent completer.
		selected := r.Config
		selected.Database.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/cockroachdb"
		selected.Console.AdminBinary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/console-admin"
		effective := c
		effective.Storage.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/aws"
		effective.Images.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/skopeo"
		files := map[string][]byte{}
		for path, b := range inputs {
			files[path] = b
		}
		files[config], _ = json.Marshal(selected)
		files[r.effectiveConfig()], _ = json.Marshal(effective)
		base, baseService, err := recoveryConfig(r.Config.RecoveryConfig)
		if err != nil {
			return err
		}
		clear(baseService.RecoveryKey)
		base.Storage.Binary = effective.Storage.Binary
		base.Images.Binary = effective.Images.Binary
		files[r.Config.RecoveryConfig], _ = json.Marshal(base)
		for _, path := range sortedFiles(files) {
			if verify {
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

func (r *Runner) renewCredentials(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var paused bool
	if err = db.QueryRowContext(ctx, "SELECT paused FROM recovery_runtime_authority WHERE singleton=TRUE").Scan(&paused); err != nil {
		return err
	}
	if paused {
		return fmt.Errorf("credential maintenance waits while runtime authority is paused")
	}
	if err = r.databaseCredentials(ctx, false); err != nil {
		return err
	}
	if err = r.credentials(ctx, false); err != nil {
		return err
	}
	for _, pl := range r.Plan.Placements {
		// A retired/offline host cannot be resurrected by credential maintenance.
		if pl.Role == deploy.Database {
			if _, err = r.remote(ctx, r.Plan, pl, "if systemctl is-active --quiet "+shell(unit(r.Plan, pl))+"; then systemctl kill --kill-who=main --signal=HUP "+shell(unit(r.Plan, pl))+"; fi\n"); err != nil {
				return err
			}
		} else if _, err = r.remote(ctx, r.Plan, pl, "if systemctl is-active --quiet "+shell(unit(r.Plan, pl))+"; then systemctl restart "+shell(unit(r.Plan, pl))+"; fi\n"); err != nil {
			return err
		}
	}
	return r.credentials(ctx, true)
}
