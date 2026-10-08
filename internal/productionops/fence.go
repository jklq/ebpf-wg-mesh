package productionops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"ebof-wg-mesh/internal/deploy"
)

func (r *Runner) priorPlan() (deploy.Plan, error) {
	if !r.Plan.Recovery || r.Plan.Previous == nil {
		return deploy.Plan{}, fmt.Errorf("fencing requires the prior deployment inventory")
	}
	old := r.Plan.Previous
	return deploy.Plan{Installation: old.Installation, Release: old.Release, Placements: old.Placements, Previous: old, Generation: r.Plan.Generation}, nil
}
func (r *Runner) verifyHostFenced(ctx context.Context, p deploy.Plan, id string) error {
	h, ok := p.Installation.Host(id)
	if !ok {
		return fmt.Errorf("prior host has no external identity")
	}
	if p.Previous != nil {
		if b := p.Previous.Bindings[id]; b.ServerID != "" {
			h.Binding = b
		}
	}
	// The explicitly selected out-of-band authority remains usable during a
	// provider API outage. Only its observed power state establishes the fence.
	if f, ok := r.Config.Fences[id]; ok && f.off(ctx) == nil {
		return nil
	}
	a, err := r.Adapter(p.Installation, h)
	if err != nil {
		return err
	}
	s, found, err := a.Discover(ctx, p.Installation.ID, h)
	if err != nil {
		return err
	}
	// Absence from a scoped provider response can mean lost visibility. Require
	// an observed power state, or independently inspect the host/external fence.
	if found && s.Status == "off" {
		if a.Capabilities().Power {
			return nil
		}
	}
	if err := r.fenceUnrecordedUnits(ctx, p, id, true); err != nil {
		return err
	}
	// Verify each prior service is stopped persistently, or has admitted the new
	// generation after a clean restart. An unreachable host is never a fence.
	for _, pl := range p.Placements {
		if pl.Host != id {
			continue
		}
		script := "if systemctl is-active --quiet " + shell(unit(p, pl)) + "; then\n"
		if mutable(pl.Role) {
			script += "cat " + shell(cfgDir(p, pl)+"/authority.json") + "\n"
		} else if pl.Role == deploy.Registry || pl.Role == deploy.Envoy {
			replacement := false
			for _, candidate := range r.Plan.Placements {
				replacement = replacement || candidate == pl
			}
			if !replacement {
				return fmt.Errorf("prior stateless instance has no admitted replacement")
			}
			script += shell(operationsBinary(r.Plan)) + " process-admission " + shell(unit(p, pl)) + " " + shell(cfgDir(p, pl)+"/authority.json") + " " + shell(r.Plan.Installation.ID) + " " + shell(r.Plan.Generation) + "\n"
		} else {
			script += "exit 1\n"
		}
		script += "else\n test \"$(systemctl is-enabled " + shell(unit(p, pl)) + " 2>/dev/null || true)\" = masked\n"
		script += "test \"$(systemctl show --value -p MainPID " + shell(unit(p, pl)) + ")\" = 0\n"
		script += "case \"$(systemctl show --value -p ActiveState " + shell(unit(p, pl)) + ")\" in inactive|failed) ;; *) exit 1;; esac\nfi\n"
		b, err := r.remote(ctx, p, pl, script)
		if err != nil {
			return fmt.Errorf("unresolved fencing of %s/%s: %w", id, pl.Instance, err)
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			var authority struct {
				InstallationID string `json:"installationId"`
				Generation     string `json:"generation"`
				Paused         bool   `json:"paused"`
			}
			if err := json.Unmarshal(b, &authority); err != nil {
				return err
			}
			if authority.InstallationID != r.Plan.Installation.ID || authority.Generation != r.Plan.Generation || !authority.Paused {
				return fmt.Errorf("prior participant is not fenced")
			}
			// Its live admission must match as well, not just the newly written file.
			probe := r.expandProbe(r.Config.Probes[pl.Role], pl)
			body, err := r.hostProbe(ctx, r.Plan, pl, probe, false)
			if err != nil {
				return err
			}
			var report struct {
				Authority *struct {
					Generation string `json:"generation"`
					Paused     bool   `json:"paused"`
				} `json:"authority"`
			}
			if err := json.Unmarshal(body, &report); err != nil || report.Authority == nil || report.Authority.Generation != r.Plan.Generation || !report.Authority.Paused {
				return fmt.Errorf("running prior participant has not admitted the replacement authority")
			}
		}
	}
	return nil
}
func maskScript(p deploy.Plan, pl deploy.Placement) string {
	return maskUnitScript(p, unit(p, pl))
}
func maskUnitScript(p deploy.Plan, name string) string {
	// Preserve the actual unit under the quarantine directory before installing
	// the persistent systemd mask; systemctl mask cannot replace a regular unit.
	path := "/etc/systemd/system/" + name
	directory := "/var/lib/ebpf-wg-mesh/" + p.Installation.ID + "/quarantine/units"
	saved := directory + "/" + name
	return "fragment=$(systemctl show --value -p FragmentPath " + shell(name) + ")\nmkdir -p " + shell(directory) + "\nchmod 0700 " + shell(directory) + "\nif test -n \"$fragment\" && test \"$fragment\" != /dev/null && test -f \"$fragment\"; then\nsaved=" + shell(saved) + "\nif test -e \"$saved\" && ! cmp -s \"$fragment\" \"$saved\"; then saved=$(mktemp " + shell(saved+".XXXXXX") + "); fi\ncp -- \"$fragment\" \"$saved\"\nchmod 0600 \"$saved\"\ncmp -s \"$fragment\" \"$saved\"\nsync -f \"$saved\"\nfi\nsystemctl stop " + shell(name) + "\ncase \"$(systemctl is-enabled " + shell(name) + " 2>/dev/null || true)\" in enabled|enabled-runtime|linked|linked-runtime|alias|indirect) systemctl disable " + shell(name) + ";; masked|masked-runtime|disabled|static|transient|generated|not-found|'') ;; *) exit 1;; esac\nln -sfn /dev/null " + shell(path) + "\nsystemctl daemon-reload\n! systemctl is-active --quiet " + shell(name) + "\n"
}

// Independent inventory can predate the latest cutover. Inspect the actual
// installation namespace, including loaded transient units and old timers,
// before treating a reachable host as fenced. Unknown resources are preserved.
func (r *Runner) fenceUnrecordedUnits(ctx context.Context, p deploy.Plan, host string, verify bool) error {
	pl := deploy.Placement{Host: host}
	prefix := "platform-" + p.Installation.ID + "-"
	pattern := shell(prefix + "*")
	b, err := r.remote(ctx, p, pl, "systemctl list-unit-files --no-legend --no-pager "+pattern+"\nsystemctl list-units --all --plain --no-legend --no-pager "+pattern+"\n")
	if err != nil {
		return fmt.Errorf("actual host fencing inventory unavailable: %w", err)
	}
	known := map[string]bool{}
	for _, plan := range []deploy.Plan{p, r.Plan} {
		for _, placement := range plan.Placements {
			if placement.Host == host {
				known[unit(plan, placement)] = true
			}
		}
	}
	unknown := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, prefix) || known[name] {
			continue
		}
		if (!strings.HasSuffix(name, ".service") && !strings.HasSuffix(name, ".timer")) || strings.ContainsAny(name, "/\\ \t\r\n") {
			return fmt.Errorf("unknown installation unit cannot be fenced: %q", name)
		}
		unknown[name] = true
	}
	names := make([]string, 0, len(unknown))
	for name := range unknown {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// New independent jobs have immutable plan-specific launch inputs. A
		// replaced unit file alone cannot admit an older still-running process.
		if content, ok := r.completionUnits()[name]; ok {
			script := verifyRemoteFile("/etc/systemd/system/"+name, []byte(content))
			if strings.HasSuffix(name, ".service") {
				script += "pid=$(systemctl show --value -p MainPID " + shell(name) + ")\nif test \"$pid\" != 0; then\ntest \"$(readlink /proc/\"$pid\"/exe)\" = " + shell(operationsBinary(r.Plan)) + "\ntr '\\000' '\\n' </proc/\"$pid\"/cmdline | grep -Fx -- " + shell(r.completionDirectory()+"/plan.json") + " >/dev/null\nfi\n"
			}
			if _, err := r.remote(ctx, p, pl, script); err == nil {
				continue
			}
		}
		if !verify {
			if _, err := r.remote(ctx, p, pl, maskUnitScript(p, name)); err != nil {
				return err
			}
		}
		script := "test \"$(systemctl is-enabled " + shell(name) + " 2>/dev/null || true)\" = masked\n"
		if strings.HasSuffix(name, ".service") {
			script += "test \"$(systemctl show --value -p MainPID " + shell(name) + ")\" = 0\n"
		}
		script += "case \"$(systemctl show --value -p ActiveState " + shell(name) + ")\" in inactive|failed) ;; *) exit 1;; esac\n"
		if _, err := r.remote(ctx, p, pl, script); err != nil {
			return fmt.Errorf("unrecorded installation unit is not fenced: %s/%s: %w", host, name, err)
		}
	}
	return nil
}
func (r *Runner) fence(ctx context.Context, verify bool) error {
	p, err := r.priorPlan()
	if err != nil {
		return err
	}
	for _, h := range p.Installation.Hosts {
		involved := false
		for _, pl := range p.Placements {
			involved = involved || pl.Host == h.ID
		}
		if !involved {
			continue
		}
		if r.verifyHostFenced(ctx, p, h.ID) == nil {
			continue
		}
		if verify {
			return fmt.Errorf("prior host %s fencing is unresolved", h.ID)
		}
		if b := p.Previous.Bindings[h.ID]; b.ServerID != "" {
			h.Binding = b
		}
		_, reused := r.Plan.Installation.Host(h.ID)
		if f, ok := r.Config.Fences[h.ID]; ok && !reused {
			if err = f.powerOff(ctx); err != nil {
				return err
			}
		} else {
			a, err := r.Adapter(p.Installation, h)
			if err != nil {
				return err
			}
			if a.Capabilities().Power && !reused {
				if err := a.Power(ctx, h.Binding.ServerID, "off"); err != nil {
					return err
				}
			} else {
				if err := r.fenceUnrecordedUnits(ctx, p, h.ID, false); err != nil {
					return err
				}
				for _, pl := range p.Placements {
					if pl.Host == h.ID {
						if _, err := r.remote(ctx, p, pl, maskScript(p, pl)); err != nil {
							return fmt.Errorf("host fencing requires reachable SSH or independently verifiable provider power: %w", err)
						}
					}
				}
			}
		}
		if err := r.verifyHostFenced(ctx, p, h.ID); err != nil {
			return err
		}
	}
	return nil
}
