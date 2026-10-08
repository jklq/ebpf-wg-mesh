package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ebof-wg-mesh/internal/recovery"
)

type SSHDriver struct {
	Remote            Remote
	Adapter           func(Installation, Host) (Adapter, error)
	RecoveryConfig    string
	recoverySelection string
	recoveryPoint     recovery.Point
	recoveryService   recovery.Service
}

func NewSSHDriver() *SSHDriver { return &SSHDriver{Remote: SSHRemote{}, Adapter: NewAdapter} }
func boundHost(i Installation, state State, id string) (Host, error) {
	h, ok := i.Host(id)
	if !ok && state.Policy != nil {
		h, ok = state.Policy.Host(id)
	}
	if !ok {
		return h, fmt.Errorf("unknown host %s", id)
	}
	if b := state.Bindings[id]; b.ServerID != "" {
		h.Binding = b
	}
	return h, nil
}
func (d *SSHDriver) discover(ctx context.Context, i Installation, state State, id string) (Host, Server, bool, error) {
	h, err := boundHost(i, state, id)
	if err != nil {
		return h, Server{}, false, err
	}
	if state.Deleted[id] {
		return h, Server{}, false, nil
	}
	a, err := d.Adapter(i, h)
	if err != nil {
		return h, Server{}, false, err
	}
	s, ok, err := a.Discover(ctx, i.ID, h)
	h.SSH.Address = strings.ReplaceAll(h.SSH.Address, "{providerAddress}", s.Address)
	return h, s, ok, err
}

const hostProbe = `arch=$(uname -m)
cpu=$(getconf _NPROCESSORS_ONLN)
ram=$(awk '/MemTotal:/{print int($2/1024)}' /proc/meminfo)
disk=$(df -BG --output=size /var/lib | tail -n 1 | tr -dc '0-9')
printf '%s\n%s\n%s\n%s\n' "$arch" "$cpu" "$ram" "$disk"
command -v systemctl >/dev/null && printf 'systemd\n' || true
command -v containerd >/dev/null && printf 'containerd\n' || true
test -f /sys/fs/cgroup/cgroup.controllers && printf 'cgroup-v2\n' || true
test -d /sys/fs/bpf && printf 'ebpf-policy\n' || true
test -d /sys/module/wireguard && printf 'wireguard\n' || true
test -c /dev/kvm && printf 'kvm\n' || true
`

func (d *SSHDriver) Inventory(ctx context.Context, i Installation, r Release, state State) (Inventory, error) {
	inv := Inventory{Hosts: map[string]HostStatus{}, Storage: map[string]StorageStatus{}}
	for _, declared := range i.Hosts {
		h, s, found, err := d.discover(ctx, i, state, declared.ID)
		if err != nil {
			return inv, err
		}
		status := HostStatus{ServerID: s.ID}
		if !found {
			inv.Hosts[h.ID] = status
			continue
		}
		// Resolve errors are configuration failures, not offline observations.
		if _, err := i.Resolve(h.SSH.Key); err != nil {
			return inv, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := d.Remote.Run(probeCtx, i, h, hostProbe)
		cancel()
		if err == nil {
			status, err = parseHostObservation(out, h.ID, s.ID)
			if err != nil {
				return inv, err
			}
		}
		inv.Hosts[h.ID] = status
	}
	if state.Bundle != nil && len(state.Placements) > 0 {
		p := Plan{Installation: i, Release: *state.Bundle, Placements: state.Placements, Previous: previousDeployment(state), Generation: state.Generation, Automatic: true}
		h, err := boundHost(i, state, administrationHost(i, inv))
		if err != nil {
			return inv, err
		}
		// The verifier reports observed members and replication, never desired
		// counts. Failure leaves redundancy unverified without shrinking members.
		if inv.Hosts[h.ID].Online {
			out, err := d.runHook(ctx, p, state, Operation{Host: h.ID, Hook: "database-verify"}, true)
			if err == nil {
				if err := json.Unmarshal(out, &inv.Database); err != nil {
					return inv, fmt.Errorf("database verifier must return DatabaseStatus JSON: %w", err)
				}
			}
			out, err = d.runHook(ctx, p, state, Operation{Host: h.ID, Hook: "storage-verify"}, true)
			if err == nil {
				if err := json.Unmarshal(out, &inv.Storage); err != nil {
					return inv, fmt.Errorf("storage verifier must return storage-status JSON: %w", err)
				}
			}
		}
		if len(inv.Database.Members) == 0 {
			for _, pl := range state.Placements {
				if pl.Role == Database {
					inv.Database.Members = append(inv.Database.Members, pl.Host)
				}
			}
		}
	}
	return inv, nil
}
func (d *SSHDriver) hook(p Plan, state State, op Operation) (Hook, Placement, error) {
	pl := Placement{Host: op.Host, Instance: "management"}
	if op.Placement != nil {
		pl = *op.Placement
	}
	if op.Kind == "convert" {
		return p.Release.Conversions[op.Hook], pl, nil
	}
	if op.Kind == "drain" || op.Kind == "database-remove" || op.Kind == "retire" {
		bundle := p.Release
		if state.Bundle != nil {
			bundle = *state.Bundle
		}
		prog := bundle.Programs[pl.Role]
		if op.Kind == "retire" {
			return prog.Retire, pl, nil
		}
		return prog.Drain, pl, nil
	}
	h, ok := p.Release.Hooks[op.Hook]
	if !ok {
		return h, pl, fmt.Errorf("unknown hook %s", op.Hook)
	}
	return h, pl, nil
}
func (d *SSHDriver) runHook(ctx context.Context, p Plan, state State, op Operation, verify bool) ([]byte, error) {
	hook, pl, err := d.hook(p, state, op)
	if err != nil {
		return nil, err
	}
	argv := hook.Command
	if verify {
		argv = hook.Verify
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("missing %s hook command/verification", op.Hook)
	}
	commandPlan := p
	if (op.Kind == "drain" || op.Kind == "retire" || op.Kind == "database-remove") && state.Bundle != nil {
		commandPlan.Release = *state.Bundle
	}
	executionHost := op.Host
	if op.Kind == "drain" || op.Kind == "retire" || op.Kind == "database-remove" {
		executionHost = p.AdministrationHost
	}
	h, _, found, err := d.discover(ctx, p.Installation, state, executionHost)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("hook host %s not found", op.Host)
	}
	// A complete plan is available to pinned management commands, containing
	// only references. Providers, database credentials and vault access are
	// provisioned separately and usable without logging into the platform.
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	path := "/etc/ebpf-wg-mesh/" + p.Installation.ID + "/plan.json"
	script := "export PLATFORM_INSTALLATION=" + quote(p.Installation.ID) + "\nexport PLATFORM_RECOVERY_GENERATION=" + quote(p.Generation) + "\n" + fileScript(path, payload, "0600") + "export PLATFORM_PLAN=" + quote(path) + "\nexport PLATFORM_OPERATION=" + quote(op.ID) + "\n"
	statePayload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	statePath := "/etc/ebpf-wg-mesh/" + p.Installation.ID + "/installer-state.json"
	script += fileScript(statePath, statePayload, "0600") + "export PLATFORM_DEPLOYMENT_STATE=" + quote(statePath) + "\nexport PLATFORM_OPERATIONS_CONFIG=" + quote(p.Installation.OperationsConfig) + "\n"
	if state.LastBackup.Backup != "" {
		script += "export PLATFORM_BACKUP=" + quote(state.LastBackup.Backup) + "\nexport PLATFORM_DATA_LOSS_CUTOFF=" + quote(state.LastBackup.DataLossCutoff.Format(time.RFC3339Nano)) + "\n"
	}
	if restorationPlan(p) && state.Recovery != nil {
		data, err := json.Marshal(state.Recovery)
		if err != nil {
			return nil, err
		}
		recoveryPath := "/etc/ebpf-wg-mesh/" + p.Installation.ID + "/recovery.json"
		script += fileScript(recoveryPath, data, "0600") + "export PLATFORM_RECOVERY_OPERATION=" + quote(recoveryPath) + "\nexport PLATFORM_RECOVERY_GENERATION=" + quote(p.Generation) + "\n"
		if state.LastRestore != nil {
			script += "export PLATFORM_BACKUP=" + quote(state.LastRestore.Backup) + "\nexport PLATFORM_DATA_LOSS_CUTOFF=" + quote(state.LastRestore.DataLossCutoff.Format(time.RFC3339Nano)) + "\n"
		}
	}
	inputs := append([]string{}, p.Installation.OperationsInputs...)
	if p.Installation.OperationsConfig != "" {
		for _, provider := range p.Installation.Providers {
			if provider.Token != "" {
				inputs = append(inputs, p.Installation.Secrets[provider.Token].File)
			}
		}
		for _, host := range append(append([]Host{}, p.Installation.Hosts...), p.Installation.Recovery.Hosts...) {
			inputs = append(inputs, host.SSH.KnownHosts, p.Installation.Secrets[host.SSH.Key].File)
		}
	}
	for _, input := range inputs {
		data, err := readSecretFile("operations-input", input)
		if err != nil {
			return nil, err
		}
		script += fileScript(input, data, "0600")
	}
	return d.Remote.Run(ctx, p.Installation, h, script+command(expandCommand(commandPlan, pl, argv))+"\n")
}
func (d *SSHDriver) Observe(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	if op.Kind == "retain" {
		return true, Evidence{}, nil
	}
	if op.Kind == "purchase" {
		_, _, found, err := d.discover(ctx, p.Installation, state, op.Host)
		return found, Evidence{}, err
	}
	if op.Kind == "delete" {
		_, _, found, err := d.discover(ctx, p.Installation, state, op.Host)
		return !found, Evidence{}, err
	}
	if op.Kind == "hook" || op.Kind == "convert" || op.Kind == "drain" || op.Kind == "retire" {
		out, err := d.runHook(ctx, p, state, op, true)
		if err != nil {
			return false, Evidence{}, nil
		}
		evidence := Evidence{}
		if contract, declared := ContractForHook(op.Hook); declared {
			var target any = &evidence
			switch contract.Kind {
			case DatabaseVerification:
				evidence.Database = &DatabaseStatus{}
				target = evidence.Database
			case StorageVerification:
				target = &evidence.Storage
			}
			if err := json.Unmarshal(out, target); err != nil {
				return false, evidence, fmt.Errorf("%s verification must return its declared evidence: %w", op.Hook, err)
			}
			if err := validateHookState(op.Hook, p, state, evidence, time.Now()); err != nil {
				return false, evidence, err
			}
		}
		return true, evidence, nil
	}
	h, server, found, err := d.discover(ctx, p.Installation, state, op.Host)
	if err != nil {
		return false, Evidence{}, err
	}
	if !found {
		return false, Evidence{}, nil
	}
	if op.Kind == "stage-tools" {
		toolPlan := toolPlan(p, state, op)
		script := "test \"$(cat " + quote(toolsDir(toolPlan)+"/staged") + ")\" = " + quote(Digest(toolPlan.Release.Tools)) + "\n"
		for name, architectures := range toolPlan.Release.Tools {
			artifact, ok := architectures[h.Architecture]
			if !ok {
				return false, Evidence{}, fmt.Errorf("management tool %s unavailable for %s", name, h.Architecture)
			}
			script += "printf %s " + quote(artifact.SHA256+"  "+toolsDir(toolPlan)+"/"+name+"\n") + " | sha256sum -c - >/dev/null\n"
		}
		_, err := d.Remote.Run(ctx, p.Installation, h, script)
		return err == nil, Evidence{}, nil
	}
	if op.Placement == nil {
		return false, Evidence{}, fmt.Errorf("operation %s requires placement", op.Kind)
	}
	pl := *op.Placement
	if op.Kind == "stage" || op.Kind == "configure" {
		marker := "staged"
		if op.Kind == "configure" {
			marker = "configured"
		}
		script := "test \"$(cat " + quote(configDir(p.Installation, pl)+"/"+marker) + ")\" = " + quote(p.ID+"/"+op.ID) + "\n"
		if op.Kind == "stage" {
			artifact := p.Release.Programs[pl.Role].Artifacts[h.Architecture]
			script += "printf %s " + quote(artifact.SHA256+"  "+releaseDir(p, pl)+"/program\n") + " | sha256sum -c - >/dev/null\n"
		} else {
			for path, ref := range p.Installation.Components[pl.Role].Secrets {
				value, err := p.resolveSecret(pl, ref)
				if err != nil {
					return false, Evidence{}, err
				}
				script += "printf %s " + quote(fmt.Sprintf("%x  %s\n", sha256.Sum256(value), configDir(p.Installation, pl)+"/"+path)) + " | sha256sum -c - >/dev/null\n"
			}
		}
		_, err := d.Remote.Run(ctx, p.Installation, h, script)
		return err == nil, Evidence{}, nil
	}
	if op.Kind == "database-remove" {
		done, _, err := d.observeDrain(ctx, p, state, op)
		if !done || err != nil {
			return done, Evidence{}, err
		}
		_, err = d.Remote.Run(ctx, p.Installation, h, "! systemctl is-active --quiet "+quote(unit(p.Installation, pl))+"\n")
		return err == nil, Evidence{}, nil
	}
	if op.Kind == "stop" {
		if server.Status == "off" {
			return true, Evidence{}, nil
		}
		_, err := d.Remote.Run(ctx, p.Installation, h, "! systemctl is-active --quiet "+quote(unit(p.Installation, pl))+"\n")
		return err == nil, Evidence{}, nil
	}
	if op.Kind == "install" || op.Kind == "database-join" {
		out, err := d.Remote.Run(ctx, p.Installation, h, hostProbe)
		if err != nil {
			return false, Evidence{}, nil
		}
		observed, err := parseHostObservation(out, h.ID, h.Binding.ServerID)
		if err != nil {
			return false, Evidence{}, err
		}
		script, err := installedScript(p, pl, observed.Capacity)
		if err != nil {
			return false, Evidence{}, err
		}
		_, err = d.Remote.Run(ctx, p.Installation, h, script)
		return err == nil, Evidence{}, nil
	}
	return false, Evidence{}, fmt.Errorf("unknown operation %s", op.Kind)
}
func (d *SSHDriver) observeDrain(ctx context.Context, p Plan, state State, op Operation) (bool, Evidence, error) {
	_, err := d.runHook(ctx, p, state, op, true)
	return err == nil, Evidence{}, nil
}
func (d *SSHDriver) Execute(ctx context.Context, p Plan, state State, op Operation) (Binding, error) {
	if op.Kind == "hook" || op.Kind == "convert" || op.Kind == "drain" || op.Kind == "retire" {
		_, err := d.runHook(ctx, p, state, op, false)
		return Binding{}, err
	}
	h, _, found, err := d.discover(ctx, p.Installation, state, op.Host)
	if err != nil {
		return Binding{}, err
	}
	if op.Kind == "purchase" {
		a, err := d.Adapter(p.Installation, h)
		if err != nil {
			return Binding{}, err
		}
		s, err := a.Create(ctx, p.Installation.ID, h)
		return Binding{Provider: h.Binding.Provider, ServerID: s.ID}, err
	}
	if op.Kind == "delete" {
		a, err := d.Adapter(p.Installation, h)
		if err != nil {
			return Binding{}, err
		}
		return Binding{}, a.Delete(ctx, h.Binding.ServerID)
	}
	if !found {
		return Binding{}, fmt.Errorf("host %s not discovered", op.Host)
	}
	if op.Kind == "stage-tools" {
		var script string
		if restorationPlan(p) {
			script, err = d.stageRecovered(ctx, toolPlan(p, state, op), state, h, nil)
		} else {
			script, err = toolsScript(toolPlan(p, state, op), h)
		}
		if err != nil {
			return Binding{}, err
		}
		_, err = d.Remote.Run(ctx, p.Installation, h, script)
		return Binding{}, err
	}
	if op.Placement == nil {
		return Binding{}, fmt.Errorf("operation requires placement")
	}
	pl := *op.Placement
	var script string
	switch op.Kind {
	case "stage":
		if restorationPlan(p) {
			script, err = d.stageRecovered(ctx, p, state, h, &pl)
		} else {
			script, err = stageScript(p, pl)
		}
		if err != nil {
			return Binding{}, err
		}
		script += fileScript(configDir(p.Installation, pl)+"/staged", []byte(p.ID+"/"+op.ID), "0600")
	case "configure":
		script, err = configurationScript(p, pl)
		if err != nil {
			return Binding{}, err
		}
		script += fileScript(configDir(p.Installation, pl)+"/configured", []byte(p.ID+"/"+op.ID), "0600")
	case "install", "database-join":
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, probeErr := d.Remote.Run(probeCtx, p.Installation, h, hostProbe)
		cancel()
		if probeErr != nil {
			return Binding{}, probeErr
		}
		observed, err := parseHostObservation(out, h.ID, h.Binding.ServerID)
		if err != nil {
			return Binding{}, err
		}
		if observed.Architecture != h.Architecture || !observed.Capacity.Fits(p.Reservations[h.ID]) {
			return Binding{}, fmt.Errorf("host %s observed architecture/capacity cannot satisfy the applied reservation", h.ID)
		}
		for _, capability := range append([]string{"systemd"}, p.Installation.Components[pl.Role].Capabilities...) {
			if !contains(observed.Capabilities, capability) {
				return Binding{}, fmt.Errorf("host %s no longer has runtime capability %s", h.ID, capability)
			}
		}
		script, err = installScript(p, pl, observed.Capacity)
		if err != nil {
			return Binding{}, err
		}
	case "stop":
		script = "systemctl disable --now " + quote(unit(p.Installation, pl)) + "\n"
	case "database-remove":
		if _, err := d.runHook(ctx, p, state, op, false); err != nil {
			return Binding{}, err
		}
		done, _, err := d.observeDrain(ctx, p, state, op)
		if err != nil {
			return Binding{}, err
		}
		if !done {
			return Binding{}, fmt.Errorf("database decommission/replication is pending; infrastructure remains intact")
		}
		script = "systemctl disable --now " + quote(unit(p.Installation, pl)) + "\n"
	default:
		return Binding{}, fmt.Errorf("unsupported operation %s", op.Kind)
	}
	_, err = d.Remote.Run(ctx, p.Installation, h, script)
	return Binding{}, err
}

func toolPlan(p Plan, state State, op Operation) Plan {
	if op.Hook == "previous" && state.Bundle != nil {
		p.Release = *state.Bundle
	}
	return p
}

func parseHostObservation(out []byte, host, server string) (HostStatus, error) {
	status := HostStatus{ServerID: server}
	lines := strings.Fields(string(out))
	if len(lines) < 4 {
		return status, fmt.Errorf("invalid resource observation from %s", host)
	}
	cpu, e1 := strconv.ParseInt(lines[1], 10, 64)
	ram, e2 := strconv.ParseInt(lines[2], 10, 64)
	disk, e3 := strconv.ParseInt(lines[3], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || cpu <= 0 || ram <= 0 || disk <= 0 {
		return status, fmt.Errorf("invalid host capacities for %s", host)
	}
	status.Online = true
	status.Architecture = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[lines[0]]
	status.Capacity = Resources{CPUMillis: cpu * 1000, MemoryMiB: ram, DiskGiB: disk}
	status.Capabilities = lines[4:]
	return status, nil
}
