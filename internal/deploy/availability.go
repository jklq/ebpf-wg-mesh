package deploy

import "fmt"

// Failure simulation includes connectivity gateways, storage owners and public
// entry points. A controller host has no special runtime dependency.
func assess(i Installation, placements []Placement, reservations map[string]Resources, inv Inventory) Availability {
	databaseHosts := map[string]bool{}
	for _, p := range placements {
		if p.Role == Database {
			databaseHosts[p.Host] = true
		}
	}
	// The platform database is a single PostgreSQL primary. It is
	// recoverable — never redundant — once the observed primary is the
	// planned host, live, and archiving WAL for point-in-time restore.
	recoverable := inv.Database.Archiving && len(databaseHosts) == 1 && len(inv.Database.Members) == 1 && len(inv.Database.Live) == 1
	for _, id := range inv.Database.Members {
		if !databaseHosts[id] {
			recoverable = false
		}
	}
	a := Availability{OneHostFailure: true, DatabaseRecoverable: recoverable}
	if !a.DatabaseRecoverable {
		a.Warnings = append(a.Warnings, "database recoverability remains unverified until the primary is live and archiving WAL")
	}
	for name, s := range i.Storage {
		if s.Replicated && !inv.Storage[name].Verified {
			a.Warnings = append(a.Warnings, "storage "+name+" replication/durability remains unverified")
		}
	}
	for _, h := range i.Hosts {
		if len(h.Network.Gateways) > 0 {
			a.Warnings = append(a.Warnings, fmt.Sprintf("%s management/runtime paths depend on gateways %v", h.ID, h.Network.Gateways))
		}
	}

	for _, failed := range i.Hosts {
		if contains(i.RetireHosts, failed.ID) {
			continue
		}
		f := Failure{Host: failed.ID}
		alive := func(id string) bool {
			h, ok := i.Host(id)
			if !ok || id == failed.ID || h.Reliability != "reliable" || (!inv.Hosts[id].Online && h.Purchase == nil) {
				return false
			}
			if len(h.Network.Gateways) > 0 {
				for _, g := range h.Network.Gateways {
					if g != failed.ID && inv.Hosts[g].Online {
						return true
					}
				}
				return false
			}
			return true
		}
		add := func(code, subject, message string) { f.Unmet = append(f.Unmet, Requirement{code, subject, message}) }
		byRole := map[Role][]Placement{}
		for _, p := range placements {
			if alive(p.Host) {
				byRole[p.Role] = append(byRole[p.Role], p)
			}
		}
		if len(byRole[Database]) == 0 {
			add("database-primary", string(Database), "the single database primary does not survive its host; recovery is a point-in-time restore")
		}
		if !recoverable {
			add("wal-archiving", string(Database), "WAL archiving has not been verified on the observed primary")
		}
		for _, role := range []Role{ControlPlane, Console, Envoy, Registry, Builder} {
			if len(byRole[role]) == 0 {
				add("replicas", string(role), "no reliable replica survives this host/path failure")
			}
		}
		// Evaluate dependencies before their consumers. Public paths must end at
		// a usable replica, including the transitive dependencies of that replica.
		usable := map[Role][]Placement{Database: byRole[Database]}
		for _, role := range []Role{Registry, ControlPlane, Console, Builder, Agent, Envoy} {
			for _, p := range byRole[role] {
				reachable := 0
				for _, q := range byRole[Database] {
					if rtt, ok := reach(i, p.Host, q.Host); ok && rtt <= i.MaxDatabaseRTTMillis {
						reachable++
					}
				}
				dependenciesOK := true
				for _, dep := range dependencies(role) {
					ok := false
					for _, q := range usable[dep] {
						if _, connected := reach(i, p.Host, q.Host); connected {
							ok = true
						}
					}
					if !ok {
						dependenciesOK = false
					}
				}
				for _, name := range i.Components[role].Storage {
					s := i.Storage[name]
					available := false
					for _, id := range s.Hosts {
						if alive(id) && ((s.Replicated && inv.Storage[name].Verified && contains(inv.Storage[name].Hosts, id)) || (!s.Replicated && id == p.Host)) {
							if _, ok := reach(i, p.Host, id); ok {
								available = true
							}
						}
					}
					if !available {
						dependenciesOK = false
					}
				}
				if (role == Agent || role == Builder || reachable >= 1) && dependenciesOK {
					usable[role] = append(usable[role], p)
				}
			}
			if len(byRole[role]) > 0 && len(usable[role]) == 0 {
				add("network-or-storage", string(role), "surviving replicas cannot reach the database primary, component dependencies or durable storage")
			}
		}
		for _, e := range i.Endpoints {
			path := len(e.Gateways) == 0
			for _, g := range e.Gateways {
				if alive(g) {
					path = true
				}
			}
			available := false
			for _, p := range usable[e.Role] {
				h, _ := i.Host(p.Host)
				if !contains(e.Hosts, p.Host) {
					continue
				}
				if len(e.Gateways) == 0 && h.Network.Public {
					available = true
				}
				for _, gateway := range e.Gateways {
					if _, connected := reach(i, gateway, p.Host); alive(gateway) && connected {
						available = true
					}
				}
			}
			if !path || !available {
				add("public-path", e.Name, "declared public endpoint has no surviving reachable replica")
			}
		}
		free := Resources{}
		agentHosts := map[string]bool{}
		for _, agent := range usable[Agent] {
			agentHosts[agent.Host] = true
		}
		for _, h := range i.Hosts {
			if alive(h.ID) && agentHosts[h.ID] {
				capacity := h.Capacity
				if observed := inv.Hosts[h.ID]; observed.Online {
					capacity = Resources{min(capacity.CPUMillis, observed.Capacity.CPUMillis), min(capacity.MemoryMiB, observed.Capacity.MemoryMiB), min(capacity.DiskGiB, observed.Capacity.DiskGiB)}
				}
				free = free.Add(capacity.Sub(reservations[h.ID]))
			}
		}
		if !free.Fits(i.Workload) {
			add("surviving-capacity", "workloads", "surviving schedulable resources cannot accommodate the declared workload")
		}
		if len(f.Unmet) > 0 {
			a.OneHostFailure = false
		}
		a.Failures = append(a.Failures, f)
	}
	return a
}
func dependencies(r Role) []Role {
	switch r {
	case Agent, Builder:
		return []Role{ControlPlane, Registry}
	case Console:
		return []Role{ControlPlane}
	case Envoy:
		return []Role{ControlPlane, Agent}
	case ControlPlane:
		return []Role{Registry}
	default:
		return nil
	}
}
