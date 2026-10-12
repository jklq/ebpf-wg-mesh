package deploy

import (
	"fmt"
	"slices"
)

// UpgradeAssessment reports what a rolling upgrade costs and what it cannot
// hide. Peak covers one overlapping process per host; limitations are the
// interruptions an operator accepts by applying the plan.
type UpgradeAssessment struct {
	Peak         map[string]Resources `json:"peak"`
	StagingBytes map[string]int64     `json:"stagingBytes"`
	Limitations  []string             `json:"limitations,omitempty"`
}

// listenerOwner is the resident continuity process for a role with stable
// listeners. Temporary overlap is accounted separately in UpgradeAssessment.
var listenerOwner = Resources{50, 32, 1}

func ComponentResources(i Installation, r Release, role Role) Resources {
	resources := i.Components[role].Resources
	if len(r.Programs[role].Listeners) > 0 {
		resources = resources.Add(listenerOwner)
	}
	return resources
}

// RollingUpgrade reports whether the plan replaces the applied release.
func (p Plan) RollingUpgrade() bool {
	_, mode := p.workflow()
	return mode == upgradeMode
}

// preserveRuntime reports whether an upgrade can leave a replica's process
// untouched. Listener roles always hand over: their configuration lives in a
// per-release directory that later credential activation addresses.
func preserveRuntime(p Plan, pl Placement) bool {
	if p.Previous == nil || len(p.Release.Programs[pl.Role].Listeners) > 0 || !p.previouslyPlaced(pl) {
		return false
	}
	old := p.Previous
	if Digest(old.Installation.Components[pl.Role]) != Digest(p.Installation.Components[pl.Role]) || Digest(old.Release.Programs[pl.Role]) != Digest(p.Release.Programs[pl.Role]) || old.Release.Dependencies[string(pl.Role)] != p.Release.Dependencies[string(pl.Role)] {
		return false
	}
	if pl.Role == Database && old.Release.Dependencies["cockroachdb"] != p.Release.Dependencies["cockroachdb"] {
		return false
	}
	if pl.Role == Builder && Digest(old.Release.Images) != Digest(p.Release.Images) {
		return false
	}
	// Stop preparation reads the replica count written at install.
	count := func(placements []Placement) int {
		return len(slices.DeleteFunc(slices.Clone(placements), func(other Placement) bool { return other.Role != pl.Role }))
	}
	if count(old.Placements) != count(p.Placements) {
		return false
	}
	h, _ := p.Installation.Host(pl.Host)
	previous, _ := old.Installation.Host(pl.Host)
	if h.Network.Address != previous.Network.Address {
		return false
	}
	if pl.Role == Agent || pl.Role == Builder {
		// Their units carry the host reservation as cgroup protection.
		reserve := previous.Reserve
		for _, other := range old.Placements {
			if other.Host == pl.Host {
				reserve = reserve.Add(ComponentResources(old.Installation, old.Release, other.Role))
			}
		}
		if reserve != p.Reservations[pl.Host] {
			return false
		}
	}
	return true
}

func (p *Plan) assessUpgrade(inv Inventory) {
	u := &UpgradeAssessment{Peak: snapshot(p.Reservations), StagingBytes: map[string]int64{}}
	p.Upgrade = u
	old := p.Previous.Release
	replicas := map[Role]int{}
	for _, pl := range p.Placements {
		replicas[pl.Role]++
	}
	for _, pl := range p.Placements {
		if preserveRuntime(*p, pl) {
			continue
		}
		prog := p.Release.Programs[pl.Role]
		h, _ := p.Installation.Host(pl.Host)
		handover := len(prog.Listeners) > 0 && p.previouslyPlaced(pl) && Digest(prog.Listeners) == Digest(old.Programs[pl.Role].Listeners) && old.Tools["continuity"][h.Architecture].SHA256 == p.Release.Tools["continuity"][h.Architecture].SHA256
		switch {
		case handover:
			// One transition per host at a time: a single extra copy of the
			// largest changed serving process, beside the resident owner.
			peak := p.Reservations[pl.Host].Add(p.Previous.Installation.Components[pl.Role].Resources)
			current := u.Peak[pl.Host]
			u.Peak[pl.Host] = Resources{max(current.CPUMillis, peak.CPUMillis), max(current.MemoryMiB, peak.MemoryMiB), max(current.DiskGiB, peak.DiskGiB)}
		case pl.Role == Agent:
			u.Limitations = append(u.Limitations, fmt.Sprintf("%s keeps workload forwarding, but its management session reconnects", pl.Instance))
		case pl.Role == Builder:
			u.Limitations = append(u.Limitations, fmt.Sprintf("%s finishes admitted builds before replacement and claims no new work until it is ready", pl.Instance))
		case replicas[pl.Role] == 1 && p.previouslyPlaced(pl):
			reason := "restarts"
			if len(prog.Listeners) > 0 {
				reason = "restarts its listener owner"
			}
			u.Limitations = append(u.Limitations, fmt.Sprintf("%s %s; as the only %s replica it is briefly unavailable", pl.Instance, reason, pl.Role))
		}
	}
	for _, h := range p.Installation.Hosts {
		seen := map[string]bool{}
		add := func(a Artifact) {
			u.StagingBytes[h.ID] += a.Bytes
			if !seen[a.SHA256] {
				u.StagingBytes[h.ID] += a.UnpackedBytes
				seen[a.SHA256] = true
			}
		}
		for _, pl := range p.Placements {
			if pl.Host == h.ID && !preserveRuntime(*p, pl) {
				add(p.Release.Programs[pl.Role].Artifacts[h.Architecture])
			}
		}
		for _, tools := range p.Release.Tools {
			add(tools[h.Architecture])
		}
		peak := u.Peak[h.ID]
		peak.DiskGiB += (u.StagingBytes[h.ID] + (1 << 30) - 1) / (1 << 30)
		u.Peak[h.ID] = peak
	}
	hosts := make([]string, 0, len(u.Peak))
	for host := range u.Peak {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	for _, host := range hosts {
		h, _ := p.Installation.Host(host)
		capacity := h.Capacity
		if v := inv.Hosts[host]; v.Online {
			capacity = Resources{min(capacity.CPUMillis, v.Capacity.CPUMillis), min(capacity.MemoryMiB, v.Capacity.MemoryMiB), min(capacity.DiskGiB, v.Capacity.DiskGiB)}
		}
		if !capacity.Fits(u.Peak[host]) {
			p.Unmet = append(p.Unmet, Requirement{"upgrade-capacity", host, "insufficient CPU/memory/storage for one safely overlapping process"})
		}
	}
}

// ConfigurationDir keeps the candidate's credentials and configuration away
// from the serving generation, including during failed or interrupted staging.
func ConfigurationDir(p Plan, pl Placement) string {
	dir := configDir(p.Installation, pl)
	if len(p.Release.Programs[pl.Role].Listeners) > 0 {
		dir += "/releases/" + p.Release.ID
	}
	return dir
}

func ContinuityDir(p Plan, pl Placement) string {
	return stateDir(p.Installation, pl) + "/continuity"
}

// ContinuityBinary is content-addressed so that a running owner's executable
// is never replaced in place.
func ContinuityBinary(p Plan, pl Placement) string {
	h, _ := p.Installation.Host(pl.Host)
	return "/opt/ebpf-wg-mesh/" + p.Installation.ID + "/runtime/continuity-" + p.Release.Tools["continuity"][h.Architecture].SHA256[:16]
}
