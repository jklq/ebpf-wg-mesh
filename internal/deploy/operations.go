package deploy

import (
	"fmt"
)

func (p *Plan) buildOperations(state State, inv Inventory) error {
	managementHost := p.AdministrationHost
	add := func(kind, host, hook string, pl *Placement) {
		op := Operation{Kind: kind, Host: host, Hook: hook, Placement: pl}
		op.ID = Digest([]any{op, len(p.Operations)})[:24]
		p.Operations = append(p.Operations, op)
		if kind == "database-join" || kind == "database-remove" || hook == "database-verify" {
			p.DatabaseChanges = append(p.DatabaseChanges, op)
		}
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
				add("purchase", h.ID, "", nil)
			}
		}
	}
	upgrade := state.Bundle != nil && Digest(*state.Bundle) != Digest(p.Release)
	conversion := upgrade && (state.Bundle.Schema != p.Release.Schema || state.Bundle.ConsoleSchema != p.Release.ConsoleSchema)
	if p.Automatic && upgrade {
		return fmt.Errorf("automatic reconciliation cannot change releases")
	}
	for _, pl := range p.Placements {
		if p.Automatic && (pl.Role == Database || pl.Role == Builder || (pl.Role == Agent && !inv.Hosts[pl.Host].Online)) {
			continue
		}
		add("stage", pl.Host, "", &pl)
	}
	if len(p.Release.Tools) > 0 {
		for _, h := range p.Installation.Hosts {
			if !contains(p.Installation.RetireHosts, h.ID) && (inv.Hosts[h.ID].Online || h.Purchase != nil) {
				add("stage-tools", h.ID, "", nil)
			}
		}
	}
	if state.Bundle != nil && state.Bundle.ID != p.Release.ID && len(state.Bundle.Tools) > 0 {
		h, _ := p.Installation.Host(managementHost)
		for name, architectures := range state.Bundle.Tools {
			if _, ok := architectures[h.Architecture]; !ok {
				return fmt.Errorf("previous release tool %s is unavailable for management host %s; select a host compatible with the old lifecycle commands", name, managementHost)
			}
		}
		add("stage-tools", managementHost, "previous", nil)
	}
	add("hook", managementHost, "recovery-protect", nil)
	if state.Bundle != nil {
		// Existing workloads must yield their reservations before additional
		// platform processes consume that capacity, including database joins.
		add("hook", managementHost, "reservations", nil)
	}
	preDrained := map[string]bool{}
	if upgrade {
		// Native workload drains need the existing control-plane owner and ingress
		// acknowledgements. Complete them before pausing or stopping that owner.
		for _, old := range state.Placements {
			current, ok := selected[old.Slot()]
			if (old.Role == Agent || old.Role == Builder) && (!ok || current.Instance != old.Instance) {
				add("drain", old.Host, "", &old)
				preDrained[old.Instance] = true
			}
		}
	}
	if upgrade {
		add("hook", managementHost, "quiesce", nil)
		add("hook", managementHost, "backup", nil)
		for _, pl := range state.Placements {
			add("stop", pl.Host, "", &pl)
		}
		if conversion {
			if !hookValid(p.Release.Conversions[state.Bundle.ID]) {
				return fmt.Errorf("schema cutover requires an explicit conversion from %s; rollback requires restore", state.Bundle.ID)
			}
		}
	}
	add("hook", managementHost, "database-credentials", nil)
	for _, pl := range p.Placements {
		if pl.Role == Database {
			if p.Automatic {
				continue
			}
			kind := "install"
			if len(inv.Database.Members) > 0 && !contains(inv.Database.Members, pl.Host) {
				kind = "database-join"
			}
			add("configure", pl.Host, "", &pl)
			add(kind, pl.Host, "", &pl)
		}
	}
	// SQL conversion needs the new database processes available while all old
	// platform processes remain stopped. No new platform process can start
	// against the old schema.
	if conversion {
		add("convert", managementHost, state.Bundle.ID, nil)
	}
	if state.Bundle == nil {
		add("hook", managementHost, "database-init", nil)
		add("hook", managementHost, "platform-bootstrap", nil)
	}
	add("hook", managementHost, "backup-schedule", nil)
	// Credential distribution/renewal runs independently of platform login.
	add("hook", managementHost, "credentials", nil)
	if state.Bundle == nil {
		add("hook", managementHost, "reservations", nil)
	}
	for _, role := range []Role{Registry, ControlPlane, Console, Builder, Agent, Envoy} {
		for _, pl := range p.Placements {
			if pl.Role == role {
				if p.Automatic && (role == Builder || (role == Agent && !inv.Hosts[pl.Host].Online)) {
					continue
				}
				add("configure", pl.Host, "", &pl)
				add("install", pl.Host, "", &pl)
			}
		}
	}
	if !p.Automatic {
		add("hook", managementHost, "database-verify", nil)
		add("hook", managementHost, "storage-verify", nil)
	}
	for _, old := range append(append([]Placement{}, state.Placements...), state.Retained...) {
		current, ok := selected[old.Slot()]
		if ok && current.Instance == old.Instance {
			continue
		}
		// Heartbeat expiry is presence, not retirement. Remember unavailable
		// instances so their independent units and credentials can be drained
		// and retired after management connectivity returns.
		if p.Automatic && !inv.Hosts[old.Host].Online {
			add("retain", old.Host, "", &old)
			continue
		}
		if old.Role == Database {
			if p.Automatic {
				continue
			}
			add("database-remove", old.Host, "", &old)
			add("retire", old.Host, "", &old)
			add("hook", managementHost, "database-verify", nil)
		} else {
			if !preDrained[old.Instance] {
				add("drain", old.Host, "", &old)
			}
			add("stop", old.Host, "", &old)
			add("retire", old.Host, "", &old)
		}
	}
	add("hook", managementHost, "production-verify", nil)
	add("hook", managementHost, "recovery-finalize", nil)
	if upgrade {
		add("hook", managementHost, "resume", nil)
	}
	for _, id := range p.Installation.RetireHosts {
		if p.Automatic {
			continue
		}
		add("delete", id, "", nil)
	}
	if !upgrade && state.Bundle == nil {
		add("hook", managementHost, "backup", nil)
	}
	return nil
}
