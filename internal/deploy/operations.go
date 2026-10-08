package deploy

import "fmt"

// Actor sequences and the phase graph describe ordering; algorithms select
// applicable hosts, membership changes and retirement actions.
var runtimeActions = []string{"configure", "install"}
var statelessRetirementActions = []string{"drain", "stop", "retire"}
var databaseRetirementActions = []string{"database-remove", "retire"}

func (p *Plan) buildOperations(state State, inv Inventory) error {
	groups := map[string][]Operation{}
	add := func(phase, kind, host, hook string, pl *Placement) {
		groups[phase] = append(groups[phase], Operation{Kind: kind, Host: host, Hook: hook, Placement: pl})
	}
	_, mode := p.workflow()
	upgrade := mode == upgradeMode
	conversion := upgrade && (state.Bundle.Schema != p.Release.Schema || state.Bundle.ConsoleSchema != p.Release.ConsoleSchema)
	if p.Automatic && state.Bundle != nil && Digest(*state.Bundle) != Digest(p.Release) {
		return fmt.Errorf("automatic reconciliation cannot change releases")
	}
	selected := map[string]Placement{}
	for _, pl := range p.Placements {
		selected[pl.Slot()] = pl
	}
	for _, h := range p.Installation.Hosts {
		if h.Purchase != nil && state.Bindings[h.ID].ServerID == "" && inv.Hosts[h.ID].ServerID == "" {
			if p.Automatic {
				p.Unmet = append(p.Unmet, Requirement{"purchase-requires-plan", h.ID, "infrastructure purchases require an applied plan"})
			} else {
				p.Purchases = append(p.Purchases, h)
				add("purchase", "purchase", h.ID, "", nil)
			}
		}
		if len(p.Release.Tools) > 0 && !contains(p.Installation.RetireHosts, h.ID) && (inv.Hosts[h.ID].Online || h.Purchase != nil) {
			add("tools", "stage-tools", h.ID, "", nil)
		}
		if !p.Automatic && contains(p.Installation.RetireHosts, h.ID) {
			add("delete", "delete", h.ID, "", nil)
		}
	}
	if state.Bundle != nil && state.Bundle.ID != p.Release.ID && len(state.Bundle.Tools) > 0 {
		h, _ := p.Installation.Host(p.AdministrationHost)
		for name, architectures := range state.Bundle.Tools {
			if _, ok := architectures[h.Architecture]; !ok {
				return fmt.Errorf("previous release tool %s is unavailable for management host %s", name, h.ID)
			}
		}
		add("prior-tools", "stage-tools", h.ID, "previous", nil)
	}
	preDrained := map[string]bool{}
	if upgrade {
		for _, old := range state.Placements {
			current, ok := selected[old.Slot()]
			if (old.Role == Agent || old.Role == Builder) && (!ok || current.Instance != old.Instance) {
				add("pre-drain", "drain", old.Host, "", &old)
				preDrained[old.Instance] = true
			}
			add("stop-prior", "stop", old.Host, "", &old)
		}
		if conversion {
			if !hookValid(p.Release.Conversions[state.Bundle.ID]) {
				return fmt.Errorf("schema cutover requires an explicit conversion from %s; rollback requires restore", state.Bundle.ID)
			}
			add("convert", "convert", p.AdministrationHost, state.Bundle.ID, nil)
		}
	}
	for _, pl := range p.Placements {
		if p.Automatic && (pl.Role == Database || pl.Role == Builder || (pl.Role == Agent && !inv.Hosts[pl.Host].Online)) {
			continue
		}
		add("stage", "stage", pl.Host, "", &pl)
		phase := string(pl.Role)
		if pl.Role == Database {
			phase = "database-runtime"
		}
		for _, action := range runtimeActions {
			kind := action
			if action == "install" && pl.Role == Database && len(inv.Database.Members) > 0 && !contains(inv.Database.Members, pl.Host) {
				kind = "database-join"
			}
			add(phase, kind, pl.Host, "", &pl)
		}
	}
	for _, old := range append(append([]Placement{}, state.Placements...), state.Retained...) {
		current, ok := selected[old.Slot()]
		if ok && current.Instance == old.Instance {
			continue
		}
		if p.Automatic && !inv.Hosts[old.Host].Online {
			add("retire", "retain", old.Host, "", &old)
			continue
		}
		actions := statelessRetirementActions
		if old.Role == Database {
			if p.Automatic {
				continue
			}
			actions = databaseRetirementActions
		}
		for _, action := range actions {
			if action == "drain" && preDrained[old.Instance] {
				continue
			}
			add("retire", action, old.Host, "", &old)
		}
		if old.Role == Database {
			add("retire", "hook", p.AdministrationHost, "database-verify", nil)
		}
	}
	ops, err := p.compileWorkflow(groups)
	if err != nil {
		return err
	}
	p.Operations, p.DatabaseChanges = ops, nil
	for _, op := range ops {
		if op.Kind == "database-join" || op.Kind == "database-remove" || op.Hook == "database-verify" {
			p.DatabaseChanges = append(p.DatabaseChanges, op)
		}
	}
	return nil
}
