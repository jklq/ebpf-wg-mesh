package recovery

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

type FleetAllocation struct {
	ID                string    `json:"id"`
	ServiceID         string    `json:"serviceId"`
	EnvironmentID     string    `json:"environmentId"`
	DeploymentID      string    `json:"deploymentId"`
	SpecRevision      int64     `json:"specRevision"`
	RolloutGeneration int64     `json:"rolloutGeneration"`
	IPv4              string    `json:"ipv4"`
	IPv6              string    `json:"ipv6"`
	CreatedAt         time.Time `json:"createdAt"`
}

type NetworkReservation struct {
	Owner         string `json:"owner"`
	EnvironmentID string `json:"environmentId,omitempty"`
	Identity      uint32 `json:"identity,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
}

type FleetHost struct {
	Resources         []FleetResource      `json:"resources,omitempty"`
	ID                string               `json:"id"` // durable agent identity; machines without agents use external host identity
	LocalStoreID      string               `json:"localStoreId"`
	Generation        string               `json:"generation"`
	Reachable         bool                 `json:"reachable"`
	Isolated          bool                 `json:"isolated"`
	Decommissioned    bool                 `json:"decommissioned"`
	AuthorityResolved bool                 `json:"authorityResolved"`
	Reservations      []NetworkReservation `json:"reservations"`
	Allocations       []FleetAllocation    `json:"allocations"`
}

type FleetResource struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

// Observed includes the latest external inventory, not just hosts in SQL.
// Unreachable hosts carry their last externally recorded reservations.
type FleetInput struct {
	DesiredNetworks  []NetworkReservation `json:"desiredNetworks"`
	DesiredResources []FleetResource      `json:"desiredResources"`
	CapturedAt       time.Time            `json:"capturedAt"`
	Desired          []FleetHost          `json:"desired"`
	Observed         []FleetHost          `json:"observed"`
	Resources        []FleetResource      `json:"resources"`
}

type FleetDifference struct {
	Kind     string `json:"kind"`
	Host     string `json:"host,omitempty"`
	Resource string `json:"resource,omitempty"`
	Action   string `json:"action"`
}

type FleetReport struct {
	Installation   string               `json:"installation"`
	Generation     string               `json:"generation"`
	Cutoff         time.Time            `json:"cutoff"`
	Release        string               `json:"release"`
	CapturedAt     time.Time            `json:"capturedAt"`
	Fenced         bool                 `json:"fenced"`
	ElapsedSeconds float64              `json:"elapsedSeconds"`
	Differences    []FleetDifference    `json:"differences"`
	Reservations   []NetworkReservation `json:"reservations"`
	AdmittedAgents []string             `json:"admittedAgents"`
	Blocked        bool                 `json:"blocked"`
}

func (r FleetReport) ApprovalDigest() string {
	r.ElapsedSeconds = 0 // displaying elapsed time must not invalidate approval
	b, _ := json.Marshal(r)
	return Digest(b)
}

func CompareFleet(input FleetInput, installation, generation, release string, cutoff, started, now time.Time, fenced bool) (FleetReport, error) {
	r := FleetReport{Installation: installation, Generation: generation, Release: release, Cutoff: cutoff, CapturedAt: input.CapturedAt, Fenced: fenced, ElapsedSeconds: now.Sub(started).Seconds(), Blocked: !fenced}
	if installation == "" || generation == "" || release == "" || cutoff.IsZero() || started.IsZero() || input.CapturedAt.Before(started) || input.CapturedAt.After(now.Add(time.Minute)) {
		return r, fmt.Errorf("recovery requires fresh external inventory and explicit authority, release and cutoff")
	}
	add := func(kind, host, resource, action string) {
		r.Differences = append(r.Differences, FleetDifference{kind, host, resource, action})
	}
	desired, observed := map[string]FleetHost{}, map[string]FleetHost{}
	allocationOwner := map[string]string{}
	addressOwner := map[string]struct{ host, allocation string }{}
	for _, h := range input.Desired {
		if h.ID == "" || desired[h.ID].ID != "" {
			return r, fmt.Errorf("duplicate or empty desired host")
		}
		desired[h.ID] = h
		for _, a := range h.Allocations {
			if a.ID == "" || allocationOwner[a.ID] != "" {
				return r, fmt.Errorf("duplicate or empty desired allocation")
			}
			allocationOwner[a.ID] = h.ID
		}
	}
	reserve := func(h FleetHost) error {
		for _, reservation := range h.Reservations {
			reservation.Owner = h.ID
			if reservation.Identity == 0 && reservation.Prefix == "" {
				return fmt.Errorf("host %s has an empty network reservation", h.ID)
			}
			if reservation.Identity != 0 && reservation.EnvironmentID == "" {
				return fmt.Errorf("network identity requires its environment identity")
			}
			if reservation.Prefix != "" {
				prefix, err := netip.ParsePrefix(reservation.Prefix)
				if err != nil {
					return fmt.Errorf("host %s has invalid reserved prefix", h.ID)
				}
				reservation.Prefix = prefix.Masked().String()
			}
			r.Reservations = append(r.Reservations, reservation)
		}
		for _, a := range h.Allocations {
			for _, raw := range []string{a.IPv4, a.IPv6} {
				if raw == "" {
					continue
				}
				address, err := netip.ParseAddr(raw)
				if err != nil {
					return fmt.Errorf("allocation %s has invalid address", a.ID)
				}
				if previous, exists := addressOwner[address.String()]; exists && (previous.host != h.ID || previous.allocation != a.ID) {
					add("network-conflict", h.ID, address.String(), "resolve address already held by "+previous.host+"/"+previous.allocation)
					r.Blocked = true
				}
				addressOwner[address.String()] = struct{ host, allocation string }{h.ID, a.ID}
				r.Reservations = append(r.Reservations, NetworkReservation{Owner: h.ID, Prefix: netip.PrefixFrom(address, address.BitLen()).String()})
			}
		}
		return nil
	}
	for _, h := range input.Desired {
		if err := reserve(h); err != nil {
			return r, err
		}
	}
	for _, reservation := range input.DesiredNetworks {
		if reservation.Identity == 0 || reservation.EnvironmentID == "" || reservation.Prefix != "" {
			return r, fmt.Errorf("desired environment network identity is invalid")
		}
		r.Reservations = append(r.Reservations, reservation)
	}
	seenAllocations := map[string]string{}
	for _, h := range input.Observed {
		if h.ID == "" || observed[h.ID].ID != "" {
			return r, fmt.Errorf("duplicate or empty observed host")
		}
		observed[h.ID] = h
		if err := reserve(h); err != nil {
			return r, err
		}
		if !h.Reachable {
			add("unreachable-host", h.ID, "", "keep isolated and protect ranges until inventory or verified decommission")
			if !h.AuthorityResolved || h.Generation != generation {
				add("unresolved-authority", h.ID, "", "keep isolated until host administration and inventory resolve authority")
			}
			if !h.Isolated && !h.Decommissioned || !h.Decommissioned && len(h.Reservations) == 0 {
				r.Blocked = true
			}
			continue
		}
		if !h.AuthorityResolved {
			add("unresolved-authority", h.ID, "", "authorize generation through host administration")
			if !h.Isolated {
				r.Blocked = true
			}
		}
		if h.AuthorityResolved && h.Generation == generation {
			if desired[h.ID].ID != "" && !h.Isolated {
				r.AdmittedAgents = append(r.AdmittedAgents, h.ID)
			}
		} else if h.AuthorityResolved {
			add("unresolved-authority", h.ID, "", "provision the selected generation")
			r.Blocked = true
		}
		want, exists := desired[h.ID]
		if !exists {
			add("unknown-host", h.ID, "", "quarantine; absence from backup never authorizes deletion")
		}
		if exists && want.LocalStoreID != "" && want.LocalStoreID != h.LocalStoreID {
			add("identity-conflict", h.ID, "", "resolve local store identity before adoption")
			r.Blocked = true
		}
		allocations := map[string]FleetAllocation{}
		for _, a := range want.Allocations {
			allocations[a.ID] = a
		}
		seen := map[string]bool{}
		for _, a := range h.Allocations {
			if a.ID == "" || seenAllocations[a.ID] != "" {
				add("allocation-conflict", h.ID, a.ID, "resolve duplicated allocation identity")
				r.Blocked = true
			}
			seenAllocations[a.ID], seen[a.ID] = h.ID, true
			newer := a.CreatedAt.After(cutoff)
			if newer {
				add("after-cutoff", h.ID, a.ID, "quarantine allocation created after recovery timestamp")
			}
			wanted, ok := allocations[a.ID]
			if !ok {
				add("quarantined-allocation", h.ID, a.ID, "preserve for explicit resolution")
				if allocationOwner[a.ID] != "" {
					r.Blocked = true
					add("allocation-conflict", h.ID, a.ID, "allocation observed on a different host")
				}
				continue
			}
			wanted.CreatedAt, a.CreatedAt = time.Time{}, time.Time{}
			if wanted == a && !newer {
				add("adopt-allocation", h.ID, a.ID, "adopt through complete restored checkpoint")
			} else {
				add("changed-allocation", h.ID, a.ID, "quarantine newer state; operator-approved reconciliation must return to restored desired state")
			}
		}
		for _, a := range want.Allocations {
			if !seen[a.ID] {
				add("missing-allocation", h.ID, a.ID, "recreate restored desired allocation after approval")
			}
		}
	}
	for _, h := range input.Desired {
		if _, ok := observed[h.ID]; !ok {
			add("missing-host", h.ID, "", "locate through external inventory or verify decommission")
			r.Blocked = true
		}
	}
	allResources := append([]FleetResource{}, input.Resources...)
	for _, host := range input.Observed {
		allResources = append(allResources, host.Resources...)
	}
	desiredResources := map[string]bool{}
	for _, resource := range input.DesiredResources {
		desiredResources[resource.Kind+"/"+resource.ID] = true
	}
	for _, resource := range allResources {
		if !desiredResources[resource.Kind+"/"+resource.ID] {
			add("quarantined-resource", "", resource.Kind+"/"+resource.ID, "preserve unknown resource for explicit resolution")
		}
		if resource.CreatedAt.After(cutoff) {
			add("after-cutoff", "", resource.Kind+"/"+resource.ID, "preserve resource created after recovery timestamp")
		}
	}
	for i, a := range r.Reservations {
		for _, b := range r.Reservations[:i] {
			if a.Identity != 0 && a.Identity == b.Identity && a.EnvironmentID != b.EnvironmentID {
				add("network-conflict", a.Owner, fmt.Sprintf("network-identity/%d", a.Identity), "resolve environment identity conflict with "+b.EnvironmentID)
				r.Blocked = true
			}
			if a.Owner != b.Owner && a.Prefix != "" && b.Prefix != "" {
				pa, _ := netip.ParsePrefix(a.Prefix)
				pb, _ := netip.ParsePrefix(b.Prefix)
				if pa.Overlaps(pb) {
					add("network-conflict", a.Owner, a.Prefix, "resolve address range conflict with "+b.Owner)
					r.Blocked = true
				}
			}
		}
	}
	slices.SortFunc(r.Differences, func(a, b FleetDifference) int { return compareJSON(a, b) })
	r.Differences = slices.Compact(r.Differences)
	slices.SortFunc(r.Reservations, func(a, b NetworkReservation) int { return compareJSON(a, b) })
	r.Reservations = slices.Compact(r.Reservations)
	slices.Sort(r.AdmittedAgents)
	return r, nil
}

func compareJSON(a, b any) int {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return slices.Compare(aa, bb)
}
