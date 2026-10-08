package deploy

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

type HostStatus struct {
	Online       bool      `json:"online"`
	ServerID     string    `json:"serverId"`
	Architecture string    `json:"architecture"`
	Capacity     Resources `json:"capacity"`
	Capabilities []string  `json:"capabilities"`
}
type DatabaseStatus struct {
	Live       []string      `json:"live"`
	Members    []string      `json:"members"` // stable host IDs, including unavailable members
	Replicated bool          `json:"replicated"`
	Ranges     []RangeStatus `json:"ranges,omitempty"`
}
type RangeStatus struct {
	ID     string   `json:"id"`
	Voters []string `json:"voters"`
}
type StorageStatus struct {
	Hosts    []string `json:"hosts"`
	Verified bool     `json:"verified"`
}
type Inventory struct {
	Hosts    map[string]HostStatus    `json:"hosts"`
	Database DatabaseStatus           `json:"database"`
	Storage  map[string]StorageStatus `json:"storage"`
}
type Placement struct {
	Role     Role   `json:"role"`
	Ordinal  int    `json:"ordinal"`
	Host     string `json:"host"`
	Instance string `json:"instance"`
}

func (p Placement) Slot() string { return fmt.Sprintf("%s/%d", p.Role, p.Ordinal) }

type Operation struct {
	Phase     string     `json:"phase"`
	Requires  []string   `json:"requires,omitempty"`
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Host      string     `json:"host,omitempty"`
	Placement *Placement `json:"placement,omitempty"`
	Hook      string     `json:"hook,omitempty"`
}
type Requirement struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}
type Failure struct {
	Host  string        `json:"host"`
	Unmet []Requirement `json:"unmet"`
}
type Availability struct {
	OneHostFailure    bool      `json:"oneHostFailure"`
	DatabaseRedundant bool      `json:"databaseRedundant"`
	Failures          []Failure `json:"failures"`
	Warnings          []string  `json:"warnings"`
}

type AppliedDeployment struct {
	Interrupted  *Plan              `json:"interruptedPlan,omitempty"`
	Installation Installation       `json:"installation"`
	Release      Release            `json:"release"`
	Bindings     map[string]Binding `json:"bindings"`
	Placements   []Placement        `json:"placements"`
}

func previousDeployment(state State) *AppliedDeployment {
	previous := &AppliedDeployment{Bindings: snapshot(state.Bindings), Placements: append(append([]Placement{}, state.Placements...), state.Retained...)}
	if state.Policy != nil && state.Bundle != nil {
		previous.Installation, previous.Release = snapshot(*state.Policy), snapshot(*state.Bundle)
	} else if state.Progress != nil && state.Progress.Plan != nil {
		previous.Installation, previous.Release = snapshot(state.Progress.Plan.Installation), snapshot(state.Progress.Plan.Release)
	} else {
		return nil
	}
	if state.Progress != nil && state.Progress.Plan != nil {
		plan := snapshot(*state.Progress.Plan)
		previous.Interrupted = &plan
	}
	return previous
}

type Plan struct {
	Recovery           bool                 `json:"recovery"`
	Generation         string               `json:"generation"`
	Previous           *AppliedDeployment   `json:"previous,omitempty"`
	AdministrationHost string               `json:"administrationHost"`
	Version            int                  `json:"version"`
	ID                 string               `json:"id"`
	CreatedAt          time.Time            `json:"createdAt"`
	Installation       Installation         `json:"installation"`
	Release            Release              `json:"release"`
	StateRevision      uint64               `json:"stateRevision"`
	StateDigest        string               `json:"stateDigest"`
	InventoryDigest    string               `json:"inventoryDigest"`
	Placements         []Placement          `json:"placements"`
	Reservations       map[string]Resources `json:"reservations"`
	Purchases          []Host               `json:"purchases"`
	DatabaseChanges    []Operation          `json:"databaseChanges"`
	Operations         []Operation          `json:"operations"`
	Unmet              []Requirement        `json:"unmet"`
	Availability       Availability         `json:"availability"`
	Automatic          bool                 `json:"automatic"`
}

func (p Plan) digest() string { p.ID = ""; return Digest(p) }
func (p Plan) Validate() error {
	if p.Version != 1 || p.ID != p.digest() {
		return fmt.Errorf("plan integrity mismatch")
	}
	if err := p.Installation.Validate(p.Release); err != nil {
		return err
	}
	return p.validateWorkflow()
}

// BuildPlan preserves all eligible placements before considering new ones. It
// never removes a database member because of presence or a missed heartbeat.
func BuildPlan(i Installation, r Release, state State, inv Inventory, automatic bool, now time.Time) (Plan, error) {
	i = snapshot(i)
	r = snapshot(r)
	p := Plan{Version: 1, CreatedAt: now.UTC(), Installation: i, Release: r, StateRevision: state.Revision, StateDigest: Digest(state), InventoryDigest: Digest(inv), Reservations: map[string]Resources{}, Automatic: automatic}
	p.Previous = previousDeployment(state)
	p.Generation = state.Generation
	if p.Generation == "" {
		p.Generation = uuid.NewSHA1(uuid.NameSpaceOID, []byte(i.ID+"/"+p.StateDigest+"/"+now.UTC().Format(time.RFC3339Nano))).String()
	}
	if err := i.Validate(r); err != nil {
		return p, err
	}
	if automatic && state.Recovery != nil && state.Recovery.CompletedAt.IsZero() {
		return p, fmt.Errorf("recovery pauses automatic placement and retirement")
	}
	if state.InstallationID != "" && state.InstallationID != i.ID {
		return p, fmt.Errorf("state belongs to installation %s", state.InstallationID)
	}
	for _, h := range i.Hosts {
		binding := state.Bindings[h.ID]
		if binding.ServerID == "" && state.Policy != nil {
			if old, ok := state.Policy.Host(h.ID); ok {
				binding = old.Binding
			}
		}
		if binding.ServerID != "" && h.Binding.ServerID != "" && binding != h.Binding {
			return p, fmt.Errorf("host %s binding changed; use a new stable host identity and an explicit retirement plan", h.ID)
		}
	}
	if state.Bundle != nil && state.Bundle.ID == r.ID && Digest(*state.Bundle) != Digest(r) {
		return p, fmt.Errorf("release %s is immutable; publish a new release identity for a changed bundle", r.ID)
	}
	if automatic && (state.Policy == nil || state.Bundle == nil || Digest(i) != Digest(*state.Policy) || Digest(r) != Digest(*state.Bundle)) {
		return p, fmt.Errorf("reconciliation requires the exact applied policy and release; apply a new plan first")
	}
	p.AdministrationHost = administrationHost(i, inv)
	for _, h := range i.Hosts {
		p.Reservations[h.ID] = h.Reserve
	}
	for _, pl := range state.Retained {
		resources := i.Components[pl.Role].Resources
		if state.Policy != nil {
			resources = state.Policy.Components[pl.Role].Resources
		}
		p.Reservations[pl.Host] = p.Reservations[pl.Host].Add(resources)
	}
	existing := map[string]Placement{}
	for _, x := range state.Placements {
		existing[x.Slot()] = x
	}
	selected := map[string]Placement{}
	// Preserve in priority order, reserving database/core resources first.
	for _, role := range Roles {
		c := i.Components[role]
		for n := 0; n < c.Replicas; n++ {
			slot := Placement{Role: role, Ordinal: n}.Slot()
			old, ok := existing[slot]
			if !ok {
				continue
			}
			keep := false
			if role == Database {
				if h, ok := i.Host(old.Host); ok {
					healthy := snapshot(inv)
					status := healthy.Hosts[h.ID]
					status.Online = true
					status.Capacity = h.Capacity
					status.Architecture = h.Architecture
					status.Capabilities = h.Capabilities
					healthy.Hosts[h.ID] = status
					keep = len(eligibility(i, r, healthy, h, c, role, n, p.Reservations, selected)) == 0
				}
			} else if automatic && (role == Agent || role == Builder) {
				keep = true
			} else if h, ok := i.Host(old.Host); ok {
				keep = len(eligibility(i, r, inv, h, c, role, n, p.Reservations, selected)) == 0
			}
			if keep {
				selected[slot] = old
				p.Placements = append(p.Placements, old)
				p.Reservations[old.Host] = p.Reservations[old.Host].Add(c.Resources)
			}
		}
	}
	for _, role := range Roles {
		c := i.Components[role]
		for n := 0; n < c.Replicas; n++ {
			x := Placement{Role: role, Ordinal: n}
			if _, ok := selected[x.Slot()]; ok {
				continue
			}
			if automatic && (role == Database || role == Agent || role == Builder) {
				p.Unmet = append(p.Unmet, Requirement{"applied-plan-required", x.Slot(), "membership or stateful worker replacement requires an applied plan"})
				continue
			}
			var candidates []Host
			explanations := ""
			for _, h := range i.Hosts {
				reasons := eligibility(i, r, inv, h, c, role, n, p.Reservations, selected)
				if len(reasons) == 0 {
					candidates = append(candidates, h)
				} else {
					explanations += fmt.Sprintf("%s: %v; ", h.ID, reasons)
				}
			}
			sort.SliceStable(candidates, func(a, b int) bool {
				x, y := candidates[a], candidates[b]
				if x.Reliability != y.Reliability {
					return x.Reliability == "reliable"
				}
				domainCount := func(h Host) int {
					n := 0
					for _, pl := range selected {
						other, _ := i.Host(pl.Host)
						if pl.Role == role && other.FailureDomain == h.FailureDomain {
							n++
						}
					}
					return n
				}
				if domainCount(x) != domainCount(y) {
					return domainCount(x) < domainCount(y)
				}
				return x.ID < y.ID
			})
			if len(candidates) == 0 {
				p.Unmet = append(p.Unmet, Requirement{"capacity", x.Slot(), "no eligible host: " + explanations})
				continue
			}
			x.Host = candidates[0].ID
			x.Instance = fmt.Sprintf("%s-%d-%s", role, n, Digest([]any{i.ID, x.Host, state.Revision})[:12])
			selected[x.Slot()] = x
			p.Placements = append(p.Placements, x)
			p.Reservations[x.Host] = p.Reservations[x.Host].Add(c.Resources)
		}
	}
	// Removed members stay in the applied inventory until decommission completes.
	for _, old := range state.Placements {
		if old.Role == Database {
			if _, ok := selected[old.Slot()]; !ok && automatic {
				p.Placements = append(p.Placements, old)
				p.Reservations[old.Host] = p.Reservations[old.Host].Add(i.Components[Database].Resources)
			}
		}
	}
	sort.Slice(p.Placements, func(a, b int) bool { return p.Placements[a].Slot() < p.Placements[b].Slot() })
	p.Availability = assess(i, p.Placements, p.Reservations, inv)
	if i.OneHostFailure && !p.Availability.OneHostFailure {
		p.Availability.Warnings = append(p.Availability.Warnings, "one-host-failure target is unmet; inspect failure scenarios")
	}
	if err := p.buildOperations(state, inv); err != nil {
		return p, err
	}
	p.ID = p.digest()
	return p, nil
}

func administrationHost(i Installation, inv Inventory) string {
	if inv.Hosts[i.ManagementHost].Online {
		return i.ManagementHost
	}
	for _, h := range i.Hosts {
		if h.Trusted && h.Reliability == "reliable" && inv.Hosts[h.ID].Online && !contains(i.RetireHosts, h.ID) {
			return h.ID
		}
	}
	return i.ManagementHost
}

func eligibility(i Installation, r Release, inv Inventory, h Host, c Component, role Role, ordinal int, reservations map[string]Resources, selected map[string]Placement) []string {
	var reasons []string
	if contains(i.RetireHosts, h.ID) {
		reasons = append(reasons, "host retirement is explicitly planned")
	}
	if !contains(h.Roles, role) {
		reasons = append(reasons, "role not permitted")
	}
	if !h.Trusted && role != Agent && role != Builder {
		reasons = append(reasons, "core role requires trust")
	}
	if ordinal < c.ReliableReplicas && h.Reliability != "reliable" {
		reasons = append(reasons, "reliable baseline required")
	}
	if len(c.Hosts) > 0 && !contains(c.Hosts, h.ID) {
		reasons = append(reasons, "host constraint")
	}
	if c.DiskClass != "" && c.DiskClass != h.DiskClass {
		reasons = append(reasons, "disk performance constraint")
	}
	status := inv.Hosts[h.ID]
	if h.Purchase == nil || status.ServerID != "" {
		if !status.Online {
			reasons = append(reasons, "host unavailable")
		}
	} else if i.Providers[h.Binding.Provider].Kind != "hetzner" {
		reasons = append(reasons, "purchase unsupported")
	}
	capacity := h.Capacity
	if status.Online {
		capacity = Resources{min(capacity.CPUMillis, status.Capacity.CPUMillis), min(capacity.MemoryMiB, status.Capacity.MemoryMiB), min(capacity.DiskGiB, status.Capacity.DiskGiB)}
		if status.Architecture != h.Architecture {
			reasons = append(reasons, "observed architecture mismatch")
		}
		for _, cap := range append([]string{"systemd"}, c.Capabilities...) {
			if !contains(status.Capabilities, cap) {
				reasons = append(reasons, "missing observed capability "+cap)
			}
		}
	}
	for _, cap := range c.Capabilities {
		if !contains(h.Capabilities, cap) {
			reasons = append(reasons, "missing declared capability "+cap)
		}
	}
	if !capacity.Sub(reservations[h.ID]).Fits(c.Resources) {
		reasons = append(reasons, "CPU/RAM/disk after reservations")
	}
	if _, ok := r.Programs[role].Artifacts[h.Architecture]; !ok {
		reasons = append(reasons, "release architecture unavailable")
	}
	for _, pl := range selected {
		if pl.Role == role {
			if pl.Host == h.ID {
				reasons = append(reasons, "replicas require distinct hosts")
			}
			other, _ := i.Host(pl.Host)
			if c.DistinctDomains && other.FailureDomain == h.FailureDomain {
				reasons = append(reasons, "failure-domain constraint")
			}
		}
		if role == Database && pl.Role == Database {
			rtt, ok := reach(i, h.ID, pl.Host)
			if !ok {
				reasons = append(reasons, "database peer unreachable")
			} else if rtt > i.MaxDatabaseRTTMillis {
				reasons = append(reasons, fmt.Sprintf("database RTT %dms exceeds %dms", rtt, i.MaxDatabaseRTTMillis))
			}
		}
	}
	for _, s := range c.Storage {
		store := i.Storage[s]
		if !store.Replicated && !contains(store.Hosts, h.ID) {
			reasons = append(reasons, "local storage belongs to another host")
		}
		if store.Replicated {
			found := false
			for _, id := range store.Hosts {
				if _, ok := reach(i, h.ID, id); ok {
					found = true
				}
			}
			if !found {
				reasons = append(reasons, "storage unreachable")
			}
		}
	}
	return reasons
}

func reach(i Installation, a, b string) (int, bool) {
	if a == b {
		return 0, true
	}
	h, ok := i.Host(a)
	if !ok {
		return 0, false
	}
	other, ok := i.Host(b)
	if !ok {
		return 0, false
	}
	x, ok := h.Network.Peers[b]
	y, reverse := other.Network.Peers[a]
	return max(x, y), ok && reverse
}
