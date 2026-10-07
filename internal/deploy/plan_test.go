package deploy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Now().UTC().Truncate(time.Second)

func fixture(n int) (Installation, Release, Inventory) {
	i := Installation{Version: 1, ID: "production", Release: "r42", ManagementHost: "a", Providers: map[string]Provider{"hetzner": {Kind: "hetzner", Token: "provider"}, "gigahost": {Kind: "gigahost", Token: "provider"}, "imported": {Kind: "linux"}}, Secrets: map[string]SecretRef{"provider": {File: "/etc/platform/provider"}, "ssh": {File: "/etc/platform/ssh"}, "recovery": {File: "/etc/platform/recovery"}, "backup-writer": {File: "/etc/platform/backup-writer"}, "recovery-key": {File: "/etc/platform/recovery.key"}}, Components: map[Role]Component{}, Storage: map[string]Storage{}, Backup: Backup{Target: "s3://backups/production", Credentials: []string{"backup-writer"}, Account: "recovery", PrimaryAccount: "production", FailureDomain: "independent", RecoveryKey: "recovery-key", Monitor: "https://monitor.example/recovery"}, Recovery: Recovery{Credentials: []string{"recovery"}}, OneHostFailure: n >= 3, Workload: Resources{500, 512, 1}, MaxDatabaseRTTMillis: 120}
	r := Release{Version: 1, ID: "r42", Configuration: 1, Protocol: 1, Schema: 42, ConsoleSchema: 3, Dependencies: map[string]string{"cockroachdb": "26.1.0"}, Images: map[string]string{"registry": "distribution@sha256:" + strings.Repeat("1", 64)}, Programs: map[Role]Program{}, Hooks: map[string]Hook{}, Conversions: map[string]Hook{}}
	inv := Inventory{Hosts: map[string]HostStatus{}, Storage: map[string]StorageStatus{}}
	var ids []string
	for x := 0; x < n; x++ {
		id := string(rune('a' + x))
		ids = append(ids, id)
		provider := []string{"hetzner", "gigahost", "imported"}[x%3]
		h := Host{ID: id, Binding: Binding{Provider: provider, ServerID: fmt.Sprint(x + 1)}, SSH: SSH{Address: "192.0.2." + fmt.Sprint(x+1), User: "root", Key: "ssh", KnownHosts: "/etc/platform/known_hosts"}, Architecture: "amd64", Capacity: Resources{8000, 32768, 500}, Reserve: Resources{500, 512, 10}, DiskClass: "ssd", Capabilities: []string{"systemd", "containerd", "wireguard", "ebpf-policy", "cgroup-v2", "kvm"}, Reliability: "reliable", Trusted: true, Roles: append([]Role{}, Roles...), FailureDomain: provider + "/" + id, Region: "oslo", Network: Network{Address: "10.0.0." + fmt.Sprint(x+1), Public: true, Peers: map[string]int{}}}
		i.Hosts = append(i.Hosts, h)
		inv.Hosts[id] = HostStatus{Online: true, ServerID: h.Binding.ServerID, Architecture: h.Architecture, Capacity: h.Capacity, Capabilities: h.Capabilities}
	}
	for x := range i.Hosts {
		for _, id := range ids {
			if id != i.Hosts[x].ID {
				i.Hosts[x].Network.Peers[id] = 5
			}
		}
	}
	rescue := i.Hosts[0]
	rescue.ID = "recovery"
	rescue.Binding = Binding{Provider: "imported", ServerID: "alternative-machine"}
	rescue.FailureDomain = "offsite"
	i.Recovery.Hosts = []Host{rescue}
	i.Storage["database"] = Storage{Path: "/var/lib/cockroach", Hosts: ids}
	i.Storage["registry"] = Storage{Path: "/var/lib/registry", Hosts: ids, Replicated: n > 1}
	i.Storage["archives"] = Storage{Path: "/var/lib/platform/archives", Hosts: ids, Replicated: n > 1}
	inv.Storage["registry"] = StorageStatus{Hosts: ids, Verified: true}
	inv.Storage["archives"] = StorageStatus{Hosts: ids, Verified: true}
	for _, role := range Roles {
		count := min(n, 2)
		reliable := 1
		if role == Database {
			count = min(n, 3)
			reliable = count
		}
		if role == Agent {
			count = n
		}
		if role == Builder {
			count = 1
			reliable = 0
		}
		c := Component{Replicas: count, ReliableReplicas: reliable, Resources: Resources{200, 256, 1}, Env: map[string]string{}}
		if role == Database {
			c.Storage = []string{"database"}
			c.DiskClass = "ssd"
		}
		if role == Registry {
			c.Storage = []string{"registry"}
		}
		if role == ControlPlane {
			c.Storage = []string{"archives"}
		}
		i.Components[role] = c
		h := Hook{Command: []string{"/opt/platformops", string(role), "{instance}"}, Verify: []string{"/opt/platformops", "verify", string(role), "{instance}"}}
		r.Programs[role] = Program{Artifacts: map[string]Artifact{"amd64": {URL: "https://releases.example.com/r42/" + string(role), SHA256: strings.Repeat("0", 64)}}, Args: []string{"--identity={instance}"}, Ready: []string{"/opt/platformops", "ready", "{instance}"}, Drain: h, Retire: h}
	}
	for _, name := range []string{"database-init", "platform-bootstrap", "database-verify", "storage-verify", "production-verify", "reservations", "credentials", "backup", "recovery-protect", "backup-schedule", "recovery-finalize", "recovery-verify", "quiesce", "resume", "restore", "recovery-fence"} {
		r.Hooks[name] = Hook{Command: []string{"/opt/platformops", name}, Verify: []string{"/opt/platformops", "verify", name}}
	}
	for _, role := range []Role{Console, Envoy, Registry} {
		i.Endpoints = append(i.Endpoints, Endpoint{Name: string(role), URL: "https://" + string(role) + ".example.com", Role: role, Hosts: ids})
	}
	inv.Database = DatabaseStatus{Members: ids[:min(n, 3)], Replicated: n >= 3}
	return i, r, inv
}
func build(t *testing.T, i Installation, r Release, state State, inv Inventory, automatic bool) Plan {
	t.Helper()
	p, err := BuildPlan(i, r, state, inv, automatic, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func applied(p Plan) State {
	return State{Version: 1, InstallationID: p.Installation.ID, Revision: 1, Bindings: map[string]Binding{}, Placements: p.Placements, Policy: &p.Installation, Bundle: &p.Release}
}
func requireComplete(t *testing.T, p Plan) {
	t.Helper()
	if len(p.Unmet) > 0 {
		t.Fatalf("unmet: %+v", p.Unmet)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceTopologies(t *testing.T) {
	t.Run("single reliable host", func(t *testing.T) {
		i, r, inv := fixture(1)
		p := build(t, i, r, State{}, inv, false)
		requireComplete(t, p)
		if p.Availability.OneHostFailure || p.Availability.DatabaseRedundant {
			t.Fatal("single-host installation claims fault tolerance")
		}
		if len(p.Placements) != len(Roles) {
			t.Fatalf("incomplete platform: %v", p.Placements)
		}
	})
	t.Run("VM plus home PC", func(t *testing.T) {
		i, r, inv := fixture(2)
		i.Hosts[1].Binding.Provider = "imported"
		i.Hosts[1].Reliability = "intermittent"
		i.Hosts[1].Trusted = false
		inv.Hosts["b"] = HostStatus{Online: true, ServerID: "2", Architecture: "amd64", Capacity: i.Hosts[1].Capacity, Capabilities: i.Hosts[1].Capabilities}
		for _, role := range []Role{Database, ControlPlane, Console, Envoy, Registry} {
			c := i.Components[role]
			c.Replicas = 1
			c.ReliableReplicas = 1
			i.Components[role] = c
		}
		c := i.Components[Builder]
		c.Hosts = []string{"b"}
		i.Components[Builder] = c
		p := build(t, i, r, State{}, inv, false)
		requireComplete(t, p)
		for _, pl := range p.Placements {
			if pl.Role == Builder && pl.Host != "b" {
				t.Fatal("home builder not used")
			}
			if pl.Role == Database && pl.Host != "a" {
				t.Fatal("intermittent database")
			}
		}
		if p.Availability.OneHostFailure {
			t.Fatal("two machines claim database fault tolerance")
		}
	})
	t.Run("redundant across Hetzner and Gigahost", func(t *testing.T) {
		i, r, inv := fixture(3)
		p := build(t, i, r, State{}, inv, false)
		requireComplete(t, p)
		if !p.Availability.OneHostFailure {
			t.Fatalf("availability: %+v", p.Availability)
		}
		providers := map[string]bool{}
		for _, pl := range p.Placements {
			if pl.Role == Database {
				h, _ := i.Host(pl.Host)
				providers[h.Binding.Provider] = true
			}
		}
		if !providers["hetzner"] || !providers["gigahost"] {
			t.Fatal(providers)
		}
	})
	t.Run("regions report quorum and latency", func(t *testing.T) {
		i, r, inv := fixture(4)
		i.Hosts[1].Region = "helsinki"
		i.Hosts[2].Region = "virginia"
		i.Hosts[2].Network.Peers["a"] = 90
		i.Hosts[0].Network.Peers["c"] = 90
		p := build(t, i, r, State{}, inv, false)
		requireComplete(t, p)
		if !strings.Contains(strings.Join(p.Availability.Warnings, " "), "cross-region database quorum") {
			t.Fatal(p.Availability.Warnings)
		}
		i.MaxDatabaseRTTMillis = 10
		for _, id := range []string{"a", "b", "d"} {
			h := 2
			i.Hosts[h].Network.Peers[id] = 90
			for x := range i.Hosts {
				if i.Hosts[x].ID == id {
					i.Hosts[x].Network.Peers["c"] = 90
				}
			}
		}
		// Pin the third member to the remote host so a different local host cannot satisfy the budget.
		c := i.Components[Database]
		c.Hosts = []string{"a", "b", "c"}
		i.Components[Database] = c
		p = build(t, i, r, State{}, inv, false)
		if len(p.Unmet) == 0 || !strings.Contains(fmt.Sprint(p.Unmet), "RTT") {
			t.Fatal("latency mismatch not explained")
		}
	})
}
func TestReliableHomePCCanHostCore(t *testing.T) {
	i, r, inv := fixture(1)
	i.Hosts[0].Binding.Provider = "imported"
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	for _, pl := range p.Placements {
		if pl.Host != "a" {
			t.Fatal(pl)
		}
	}
}
func TestDeterminismPreservationAndReservations(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	requireComplete(t, p)
	if p.ID != build(t, i, r, State{}, inv, false).ID {
		t.Fatal("nondeterministic plan")
	}
	state := applied(p)
	next := build(t, i, r, state, inv, false)
	if Digest(p.Placements) != Digest(next.Placements) {
		t.Fatal("valid placements moved")
	}
	for _, h := range i.Hosts {
		expected := h.Reserve
		for _, pl := range p.Placements {
			if pl.Host == h.ID {
				expected = expected.Add(i.Components[pl.Role].Resources)
			}
		}
		if next.Reservations[h.ID] != expected {
			t.Fatalf("reservation %s", h.ID)
		}
	}
}
func TestAutomaticStatelessReplacementRetainsUnavailableEnvoy(t *testing.T) {
	i, r, inv := fixture(3)
	p := build(t, i, r, State{}, inv, false)
	state := applied(p)
	status := inv.Hosts["a"]
	status.Online = false
	inv.Hosts["a"] = status
	p = build(t, i, r, state, inv, true)
	requireComplete(t, p)
	retained := false
	for _, op := range p.Operations {
		if op.Kind == "purchase" || op.Kind == "database-remove" || op.Kind == "database-join" {
			t.Fatal("automatic database/infrastructure change", op)
		}
		if op.Kind == "retire" && op.Host == "a" {
			t.Fatal("heartbeat expiry retired unavailable member")
		}
		if op.Kind == "retain" && op.Placement.Role == Envoy {
			retained = true
		}
	}
	if !retained {
		t.Fatal("unavailable Envoy lost its safety barrier")
	}
	for _, pl := range p.Placements {
		if pl.Role == Database && pl.Ordinal == 0 && pl.Host != "a" {
			t.Fatal("outage removed database member")
		}
	}
}
func TestControlledExpansionAndVerifiedRedundancy(t *testing.T) {
	i, r, inv := fixture(3)
	c := i.Components[Database]
	c.Replicas = 1
	c.ReliableReplicas = 1
	i.Components[Database] = c
	inv.Database = DatabaseStatus{Members: []string{"a"}}
	p := build(t, i, r, State{}, inv, false)
	state := applied(p)
	c.Replicas = 3
	c.ReliableReplicas = 3
	i.Components[Database] = c
	p = build(t, i, r, state, inv, false)
	requireComplete(t, p)
	joins := 0
	for _, op := range p.DatabaseChanges {
		if op.Kind == "database-join" {
			joins++
		}
	}
	if joins != 2 {
		t.Fatal("expected native joins", p.DatabaseChanges)
	}
	if p.Availability.DatabaseRedundant {
		t.Fatal("desired counts reported as completed replication")
	}
	inv.Database = DatabaseStatus{Members: []string{"a", "b", "c"}, Replicated: true}
	p = build(t, i, r, state, inv, false)
	if !p.Availability.DatabaseRedundant {
		t.Fatal("verified redundancy not reported")
	}
	if _, err := BuildPlan(i, r, state, inv, true, testNow); err == nil {
		t.Fatal("unapplied policy reconciled")
	}
}
func TestAvailabilityIncludesGatewayStorageIngressAndCapacity(t *testing.T) {
	for _, scenario := range []struct {
		name, code string
		change     func(*Installation)
	}{
		{"gateway", "quorum", func(i *Installation) {
			i.Hosts[1].Network.Gateways = []string{"a"}
			i.Hosts[2].Network.Gateways = []string{"a"}
		}},
		{"registry storage", "network-or-storage", func(i *Installation) {
			s := i.Storage["registry"]
			s.Hosts = []string{"a"}
			s.Replicated = false
			i.Storage["registry"] = s
		}},
		{"public ingress", "public-path", func(i *Installation) {
			for n := range i.Endpoints {
				if i.Endpoints[n].Role == Envoy {
					i.Endpoints[n].Hosts = []string{"a"}
				}
			}
		}},
		{"surviving workload capacity", "surviving-capacity", func(i *Installation) { i.Workload.CPUMillis = 50000 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			i, r, inv := fixture(3)
			initial := build(t, i, r, State{}, inv, false)
			scenario.change(&i)
			a := assess(i, initial.Placements, initial.Reservations, inv)
			if a.OneHostFailure {
				t.Fatal("invalid full-platform availability claim")
			}
			found := false
			for _, f := range a.Failures {
				for _, u := range f.Unmet {
					if u.Code == scenario.code {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("missing %s: %+v", scenario.code, a)
			}
		})
	}
}
func TestInsufficientCapacityExplainsReservationsTrustAndCapabilities(t *testing.T) {
	i, r, inv := fixture(1)
	i.Hosts[0].Reserve = i.Hosts[0].Capacity
	p := build(t, i, r, State{}, inv, false)
	if len(p.Unmet) == 0 || !strings.Contains(fmt.Sprint(p.Unmet), "after reservations") {
		t.Fatal(p.Unmet)
	}
	i, r, inv = fixture(1)
	i.Hosts[0].Trusted = false
	if _, err := BuildPlan(i, r, State{}, inv, false, testNow); err == nil {
		t.Fatal("untrusted management host")
	}
	i, r, inv = fixture(1)
	c := i.Components[Agent]
	c.Capabilities = []string{"cgroup-v2"}
	i.Components[Agent] = c
	status := inv.Hosts["a"]
	status.Capabilities = []string{"systemd"}
	inv.Hosts["a"] = status
	p = build(t, i, r, State{}, inv, false)
	if !strings.Contains(fmt.Sprint(p.Unmet), "missing observed capability") {
		t.Fatal(p.Unmet)
	}
}
