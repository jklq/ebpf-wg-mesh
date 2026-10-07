package productionops

import (
	"context"
	"encoding/json"
	"fmt"
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
	a, err := r.Adapter(p.Installation, h)
	if err != nil {
		return err
	}
	s, found, err := a.Discover(ctx, p.Installation.ID, h)
	if err != nil {
		return err
	}
	if !found || s.Status == "off" {
		if a.Capabilities().Power {
			return nil
		}
	}
	if f, ok := r.Config.Fences[id]; ok && f.off(ctx) == nil {
		return nil
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
		script += "else\n test \"$(systemctl is-enabled " + shell(unit(p, pl)) + " 2>/dev/null || true)\" = masked\nfi\n"
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
	// Preserve the actual unit under the quarantine directory before installing
	// the persistent systemd mask; systemctl mask cannot replace a regular unit.
	path := "/etc/systemd/system/" + unit(p, pl)
	saved := "/var/lib/ebpf-wg-mesh/" + p.Installation.ID + "/quarantine/units/" + unit(p, pl)
	return "systemctl disable --now " + shell(unit(p, pl)) + "\nmkdir -p " + shell(strings.TrimSuffix(saved, "/"+unit(p, pl))) + "\nif test -f " + shell(path) + " && ! test -L " + shell(path) + "; then mv " + shell(path) + " " + shell(saved) + "; fi\nln -sfn /dev/null " + shell(path) + "\nsystemctl daemon-reload\n! systemctl is-active --quiet " + shell(unit(p, pl)) + "\n"
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
		a, err := r.Adapter(p.Installation, h)
		if err != nil {
			return err
		}
		_, reused := r.Plan.Installation.Host(h.ID)
		if a.Capabilities().Power && !reused {
			if err := a.Power(ctx, h.Binding.ServerID, "off"); err != nil {
				return err
			}
		} else if f, ok := r.Config.Fences[h.ID]; ok && !reused {
			if err = f.powerOff(ctx); err != nil {
				return err
			}
		} else {
			for _, pl := range p.Placements {
				if pl.Host == h.ID {
					if _, err := r.remote(ctx, p, pl, maskScript(p, pl)); err != nil {
						return fmt.Errorf("host fencing requires reachable SSH or independently verifiable provider power: %w", err)
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
