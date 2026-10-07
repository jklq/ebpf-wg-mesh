package deploy

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

func durable(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, base := range []string{"/tmp", "/var/tmp", "/run", "/dev/shm"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return false
		}
	}
	return true
}
func hookValid(h Hook) bool { return len(h.Command) > 0 && len(h.Verify) > 0 }
func (i Installation) Validate(r Release) error {
	if i.Version != 1 || r.Version != 1 || !safeID(i.ID) || !safeID(r.ID) || i.Release != r.ID {
		return fmt.Errorf("installation/release version, identity, or binding is invalid")
	}
	if r.Configuration <= 0 || r.Protocol <= 0 || r.Schema <= 0 || r.ConsoleSchema <= 0 {
		return fmt.Errorf("release must pin configuration, protocol and both schema versions")
	}
	for name, architectures := range r.Tools {
		if !safeID(name) || len(architectures) == 0 {
			return fmt.Errorf("invalid management tool %s", name)
		}
		for arch, a := range architectures {
			d, err := hex.DecodeString(a.SHA256)
			u, e := url.Parse(a.URL)
			if (arch != "amd64" && arch != "arm64") || err != nil || len(d) != 32 || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return fmt.Errorf("tool %s/%s requires pinned HTTPS artifact", name, arch)
			}
		}
	}
	for name, v := range r.Dependencies {
		if name == "" || v == "" || v == "latest" {
			return fmt.Errorf("dependency %s must be pinned", name)
		}
	}
	for name, v := range r.Images {
		if !strings.Contains(v, "@sha256:") || len(strings.Split(v, "@sha256:")[1]) != 64 {
			return fmt.Errorf("image %s must use a sha256 digest", name)
		}
	}
	for name, p := range i.Providers {
		if !safeID(name) || !contains([]string{"hetzner", "gigahost", "linux"}, p.Kind) {
			return fmt.Errorf("unknown provider %s", name)
		}
		if p.Kind != "linux" {
			if _, ok := i.Secrets[p.Token]; !ok {
				return fmt.Errorf("provider %s requires an external token reference", name)
			}
		}
	}
	for name, s := range i.Secrets {
		if name == "" || !filepath.IsAbs(s.File) {
			return fmt.Errorf("secret %s requires an absolute external file path", name)
		}
	}
	seen := map[string]bool{}
	for _, h := range append(append([]Host{}, i.Hosts...), i.Recovery.Hosts...) {
		if !safeID(h.ID) || seen[h.ID] {
			return fmt.Errorf("host identity %q is invalid or repeated", h.ID)
		}
		seen[h.ID] = true
		p, ok := i.Providers[h.Binding.Provider]
		if !ok {
			return fmt.Errorf("host %s has unknown provider", h.ID)
		}
		if h.Binding.ServerID == "" && h.Purchase == nil {
			return fmt.Errorf("host %s requires a provider binding or purchase", h.ID)
		}
		if h.Purchase != nil && (p.Kind != "hetzner" || h.Binding.ServerID != "" || h.Purchase.ServerType == "" || h.Purchase.Location == "" || h.Purchase.Image == "" || len(h.Purchase.SSHKeys) == 0) {
			return fmt.Errorf("host %s: only Hetzner supports a fully specified purchase; import purchased Gigahost servers", h.ID)
		}
		if h.Architecture != "amd64" && h.Architecture != "arm64" {
			return fmt.Errorf("host %s requires amd64 or arm64 architecture", h.ID)
		}
		if h.Capacity.CPUMillis <= 0 || h.Capacity.MemoryMiB <= 0 || h.Capacity.DiskGiB <= 0 || !h.Reserve.valid() || !h.Capacity.Fits(h.Reserve) {
			return fmt.Errorf("host %s has invalid capacity/reservations", h.ID)
		}
		if h.FailureDomain == "" || h.Region == "" || h.Network.Address == "" || h.DiskClass == "" || !contains([]string{"reliable", "intermittent"}, h.Reliability) {
			return fmt.Errorf("host %s requires reliability, disk and failure/network domains", h.ID)
		}
		if h.SSH.Address == "" || strings.HasPrefix(h.SSH.Address, "-") || h.SSH.User == "" || strings.ContainsAny(h.SSH.User, " @\n") || !filepath.IsAbs(h.SSH.KnownHosts) {
			return fmt.Errorf("host %s requires SSH endpoint, user and absolute known-hosts file", h.ID)
		}
		if _, ok := i.Secrets[h.SSH.Key]; !ok {
			return fmt.Errorf("host %s requires SSH key reference", h.ID)
		}
		for _, role := range h.Roles {
			if !contains(Roles, role) {
				return fmt.Errorf("host %s: unknown role %s", h.ID, role)
			}
		}
	}
	management, ok := i.Host(i.ManagementHost)
	if !ok || !management.Trusted || management.Reliability != "reliable" {
		return fmt.Errorf("managementHost must be a trusted reliable installation host")
	}
	for _, id := range i.RetireHosts {
		h, ok := i.Host(id)
		if !ok || i.Providers[h.Binding.Provider].Kind == "linux" || h.Purchase != nil || id == i.ManagementHost {
			return fmt.Errorf("host %s retirement requires a bound deletable provider host and a surviving managementHost", id)
		}
	}
	for _, h := range i.Hosts {
		for name, architectures := range r.Tools {
			if _, ok := architectures[h.Architecture]; !ok {
				return fmt.Errorf("management tool %s unavailable for host %s architecture", name, h.ID)
			}
		}
		for _, g := range h.Network.Gateways {
			gateway, ok := i.Host(g)
			if !ok || g == h.ID || len(gateway.Network.Gateways) > 0 {
				return fmt.Errorf("host %s has an invalid or chained gateway %s", h.ID, g)
			}
		}
		for peer, rtt := range h.Network.Peers {
			if _, ok := i.Host(peer); !ok || rtt < 0 {
				return fmt.Errorf("host %s has invalid peer %s", h.ID, peer)
			}
		}
	}
	for name, s := range i.Storage {
		if !safeID(name) || !durable(s.Path) || len(s.Hosts) == 0 {
			return fmt.Errorf("storage %s must be persistent and have declared storage hosts", name)
		}
		for _, id := range s.Hosts {
			if _, ok := i.Host(id); !ok {
				return fmt.Errorf("storage %s: unknown host %s", name, id)
			}
		}
		if s.Replicated && len(s.Hosts) < 2 {
			return fmt.Errorf("storage %s replication requires multiple hosts", name)
		}
	}
	for role, c := range i.Components {
		if !contains(Roles, role) || c.Replicas < 0 || c.ReliableReplicas < 0 || c.ReliableReplicas > c.Replicas || !c.Resources.valid() || c.Resources.CPUMillis == 0 || c.Resources.MemoryMiB == 0 {
			return fmt.Errorf("invalid component policy %s", role)
		}
		if contains([]Role{Database, ControlPlane, Console, Envoy, Registry}, role) && c.ReliableReplicas == 0 {
			return fmt.Errorf("%s requires a reliable baseline replica", role)
		}
		if role == Database && c.ReliableReplicas != c.Replicas {
			return fmt.Errorf("database membership requires reliable hosts")
		}
		p, ok := r.Programs[role]
		if !ok || len(p.Ready) == 0 {
			return fmt.Errorf("release requires a program and readiness command for %s", role)
		}
		for arch, a := range p.Artifacts {
			d, err := hex.DecodeString(a.SHA256)
			u, e := url.Parse(a.URL)
			if (arch != "amd64" && arch != "arm64") || err != nil || len(d) != 32 || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return fmt.Errorf("%s/%s requires an HTTPS executable artifact with sha256", role, arch)
			}
		}
		if !hookValid(p.Drain) || !hookValid(p.Retire) {
			return fmt.Errorf("%s requires observed drain and retirement protocols", role)
		}
		if role == Database && len(c.Storage) == 0 {
			return fmt.Errorf("database requires durable storage")
		}
		for _, s := range c.Storage {
			if _, ok := i.Storage[s]; !ok {
				return fmt.Errorf("%s references unknown storage %s", role, s)
			}
		}
		for _, id := range c.Hosts {
			if _, ok := i.Host(id); !ok {
				return fmt.Errorf("%s references unknown host %s", role, id)
			}
		}
		for path, ref := range c.Secrets {
			if filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "../") || path == ".." || path == "." {
				return fmt.Errorf("%s has unsafe secret destination", role)
			}
			if _, ok := i.Secrets[ref]; !ok {
				return fmt.Errorf("%s has unknown secret %s", role, ref)
			}
		}
		for key, value := range c.Env {
			if key == "" || strings.ContainsAny(key, "=\n\r ") || strings.ContainsAny(value, "\n\r") {
				return fmt.Errorf("%s has invalid environment entry", role)
			}
		}
		for key, ref := range c.SecretEnv {
			if key == "" || strings.ContainsAny(key, "=\n\r ") {
				return fmt.Errorf("%s has invalid secret environment key", role)
			}
			if _, ok := i.Secrets[ref]; !ok {
				return fmt.Errorf("%s has unknown secret environment reference %s", role, ref)
			}
		}
	}
	for _, role := range []Role{Database, ControlPlane, Console, Envoy, Registry} {
		if i.Components[role].Replicas < 1 {
			return fmt.Errorf("complete installation requires %s", role)
		}
	}
	for _, name := range []string{"database-init", "platform-bootstrap", "database-verify", "storage-verify", "production-verify", "reservations", "credentials", "backup", "quiesce", "resume", "restore", "recovery-fence"} {
		if !hookValid(r.Hooks[name]) {
			return fmt.Errorf("release requires idempotent %s hook with verification", name)
		}
	}
	if i.Backup.Target == "" || i.Backup.MaxAgeHours < 1 || len(i.Backup.Credentials) == 0 || len(i.Recovery.Hosts) == 0 || len(i.Recovery.Credentials) == 0 {
		return fmt.Errorf("backup policy and independent recovery inventory/credentials are required")
	}
	for _, name := range []string{"database", "keyring", "registry", "deployment-state", "volumes"} {
		if !contains(i.Backup.Includes, name) {
			return fmt.Errorf("complete backup must include %s", name)
		}
	}
	for _, ref := range append(append([]string{}, i.Backup.Credentials...), i.Recovery.Credentials...) {
		if _, ok := i.Secrets[ref]; !ok {
			return fmt.Errorf("unknown recovery credential %s", ref)
		}
	}
	if !i.Workload.valid() || i.MaxDatabaseRTTMillis < 1 {
		return fmt.Errorf("workload resources and database latency budget are required")
	}
	seenEndpoints := map[string]bool{}
	for _, e := range i.Endpoints {
		u, err := url.Parse(e.URL)
		if !safeID(e.Name) || seenEndpoints[e.Name] || err != nil || u.Scheme != "https" || u.Host == "" || !contains(Roles, e.Role) || len(e.Hosts) == 0 {
			return fmt.Errorf("invalid public endpoint %s", e.Name)
		}
		seenEndpoints[e.Name] = true
		for _, id := range append(append([]string{}, e.Hosts...), e.Gateways...) {
			if _, ok := i.Host(id); !ok {
				return fmt.Errorf("endpoint %s references unknown host %s", e.Name, id)
			}
		}
	}
	for _, role := range []Role{Console, Envoy, Registry} {
		found := false
		for _, e := range i.Endpoints {
			if e.Role == role {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("public endpoint required for %s", role)
		}
	}
	return nil
}
