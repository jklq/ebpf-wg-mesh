package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sandboxBackendName is the only sandbox backend selection. Unknown
// backends fail validation: execution never silently falls back to a
// weaker backend.
const sandboxBackendName = "containerd"

// sandboxBuildRoot is the guest path the execution workspace is
// mounted at inside every build sandbox. Host paths under the
// workspace root translate 1:1 beneath it (see guestWorkspacePath),
// so the sandbox needs no knowledge of host layout.
const sandboxBuildRoot = "/build"

// SandboxBackend runs build steps inside one-shot sandboxes. It is the
// narrow 2.4b backend seam: workspace lifecycle, snapshot handling,
// credential scoping, and cleanup stay in the BuildExecutor; the
// backend only provides containment primitives (namespaces, mounts,
// limits, network policy) and reaps its own crashed state.
type SandboxBackend interface {
	// SetupNet creates the execution network: a private network
	// namespace that is loopback-only when policy denies general
	// egress, or attached to the configured CNI network with
	// denied-CIDR blackholes otherwise. It never falls back to the
	// host network: setup failure aborts the build.
	SetupNet(ctx context.Context, buildID string, policy NetworkPolicy) (SandboxNet, error)
	// RunStep runs one command in a fresh one-shot sandbox attached
	// to net and streams output to step.OnLog. It kills the sandbox
	// on context cancellation and removes the container and its
	// snapshot afterwards.
	RunStep(ctx context.Context, net SandboxNet, step SandboxStep) error
	// TeardownNet removes execution network state. It is best
	// effort and reports joined errors.
	TeardownNet(ctx context.Context, net SandboxNet) error
	// ReapStale removes sandboxes and networks left behind by dead
	// builders and returns the number reclaimed.
	ReapStale(ctx context.Context) (int, error)
	// Close releases backend resources.
	Close() error
}

// SandboxBackendConfig carries the backend's explicit inputs. It
// mirrors config.BuilderSandboxConfig plus the builder work dir,
// which scopes netns state.
type SandboxBackendConfig struct {
	Socket          string
	Namespace       string
	Image           string
	Runtime         string
	Snapshotter     string
	CNIPluginDir    string
	CNIConfDir      string
	CNINetwork      string
	Nameservers     []string
	BuildkitdBinary string
	WorkDir         string
}

// SandboxNet is an execution-scoped network created by SetupNet.
type SandboxNet struct {
	BuildID string
	// Path is the network namespace path sandboxes join.
	Path string
	// Attached reports whether the namespace is attached to the CNI
	// network (general egress) or loopback-only.
	Attached bool
}

// SandboxStep is one command run inside a sandbox.
type SandboxStep struct {
	// Name identifies the step for container naming and logs.
	Name string
	// Argv is the command and arguments, resolved inside the
	// sandbox image.
	Argv []string
	// Env is the complete explicit environment. The backend never
	// inherits the builder process environment.
	Env []string
	// Dir is the guest working directory.
	Dir string
	// Limits are the execution resource limits.
	Limits ResourceLimits
	// Mounts are host-to-guest bind mounts. Every destination must
	// be the sandbox build root, a path beneath it, or a resolver
	// file; nothing else from the host may enter.
	Mounts []SandboxMount
	// OnLog receives streamed output lines.
	OnLog func(commandOutputLine)
}

// SandboxMount is one host-to-guest bind mount.
type SandboxMount struct {
	Source   string
	Dest     string
	ReadOnly bool
}

// SandboxStepError reports a sandboxed step that ran and exited
// nonzero. Tail carries the step's bounded output so the executor can
// classify the failure (build vs push) the same way it classifies
// host-child failures.
type SandboxStepError struct {
	Step     string
	ExitCode int
	Tail     string
}

func (e *SandboxStepError) Error() string {
	return fmt.Sprintf("sandbox step %q exited with code %d", e.Step, e.ExitCode)
}

// NewSandboxBackend opens the configured sandbox backend. Only the
// containerd backend exists; unknown backends and unsupported
// platforms fail closed.
func NewSandboxBackend(cfg SandboxBackendConfig) (SandboxBackend, error) {
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("sandbox image is required for the hardened executor")
	}
	return newSandboxBackendPlatform(cfg)
}

// validateSandboxMounts rejects any mount that would expose host
// state beyond the execution workspace and the rendered resolver
// files. Destinations are confined to the sandbox build root (and
// paths beneath it) plus /etc/resolv.conf and /etc/hosts; sources
// must be absolute host paths that exist.
func validateSandboxMounts(mounts []SandboxMount) error {
	if len(mounts) == 0 {
		return errors.New("sandbox requires at least the workspace mount")
	}
	seen := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		source := filepath.Clean(mount.Source)
		dest := filepath.Clean(mount.Dest)
		if !filepath.IsAbs(source) {
			return fmt.Errorf("sandbox mount source %q must be absolute", mount.Source)
		}
		if dest != sandboxBuildRoot &&
			!strings.HasPrefix(dest, sandboxBuildRoot+"/") &&
			dest != "/etc/resolv.conf" && dest != "/etc/hosts" {
			return fmt.Errorf("sandbox mount destination %q is outside the build root", mount.Dest)
		}
		if _, err := os.Stat(source); err != nil {
			return fmt.Errorf("sandbox mount source %q: %w", mount.Source, err)
		}
		if _, ok := seen[dest]; ok {
			return fmt.Errorf("sandbox mount destination %q is mounted twice", mount.Dest)
		}
		seen[dest] = struct{}{}
	}
	if _, ok := seen[sandboxBuildRoot]; !ok {
		return fmt.Errorf("sandbox requires a mount at %q", sandboxBuildRoot)
	}
	return nil
}

// guestWorkspacePath translates a host path under the workspace root
// to its guest path inside the sandbox. Paths outside the root are
// rejected: the sandbox only ever sees the workspace.
func guestWorkspacePath(workspaceRoot, hostPath string) (string, error) {
	root := filepath.Clean(workspaceRoot)
	target := filepath.Clean(hostPath)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("host path %q is outside the execution workspace", hostPath)
	}
	if rel == "." {
		return sandboxBuildRoot, nil
	}
	return sandboxBuildRoot + "/" + filepath.ToSlash(rel), nil
}

// sandboxContainerID derives a containerd-safe container ID from a
// build ID and step name. Container IDs accept a narrower charset
// than build IDs, so anything outside [A-Za-z0-9_.-] is replaced.
func sandboxContainerID(buildID, step string) (string, error) {
	raw := "build-" + strings.TrimSpace(buildID) + "-" + strings.TrimSpace(step)
	if strings.TrimSpace(buildID) == "" || strings.TrimSpace(step) == "" {
		return "", errors.New("sandbox container id requires a build id and step name")
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String(), nil
}

// deniedRoutePrefixes parses the execution's denied CIDRs into route
// targets. Invalid CIDRs fail the build rather than silently
// widening egress.
func deniedRoutePrefixes(policy NetworkPolicy) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(policy.DeniedCIDRs))
	for _, raw := range policy.DeniedCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("denied egress CIDR %q: %w", raw, err)
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

// renderSandboxResolvConf renders the resolver configuration mounted
// into sandboxes. Explicit nameservers win; otherwise the builder
// host's resolvers are inherited with loopback entries dropped,
// because a private network namespace cannot reach the host's
// loopback resolver. Loopback-only inheritance with general egress
// fails closed and tells the operator to configure nameservers.
func renderSandboxResolvConf(hostResolvConf []byte, nameservers []string, allowEgress bool) ([]byte, error) {
	if len(nameservers) > 0 {
		var b strings.Builder
		b.WriteString("# sandbox resolvers (operator-configured)\n")
		for _, server := range nameservers {
			addr, err := netip.ParseAddr(strings.TrimSpace(server))
			if err != nil {
				return nil, fmt.Errorf("sandbox nameserver %q: %w", server, err)
			}
			fmt.Fprintf(&b, "nameserver %s\n", addr)
		}
		return []byte(b.String()), nil
	}
	var inherited []string
	for _, line := range strings.Split(string(hostResolvConf), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(fields[1]))
		if err != nil || addr.IsLoopback() {
			continue
		}
		inherited = append(inherited, addr.String())
	}
	if len(inherited) == 0 {
		if !allowEgress {
			return []byte("# no sandbox egress: empty resolver configuration\n"), nil
		}
		return nil, errors.New("builder host has no non-loopback nameserver: set builder.sandbox.nameservers for sandboxed builds with general egress")
	}
	var b strings.Builder
	b.WriteString("# sandbox resolvers (inherited from builder host, loopback dropped)\n")
	for _, addr := range inherited {
		fmt.Fprintf(&b, "nameserver %s\n", addr)
	}
	return []byte(b.String()), nil
}

// renderSandboxHostsFile renders the minimal hosts file mounted into
// sandboxes. It carries only loopback and the sandbox hostname: no
// host entries leak in.
func renderSandboxHostsFile() []byte {
	return []byte("127.0.0.1 localhost\n::1 localhost\n127.0.0.1 build\n::1 build\n")
}

// selectCNIConfig finds the CNI configuration file for the named
// network. Selection is by exact network name, never by directory
// order, so a build sandbox can never accidentally join the workload
// mesh (or any other network) because of file sort order.
func selectCNIConfig(confDir, network string) (path string, isList bool, err error) {
	network = strings.TrimSpace(network)
	if network == "" {
		return "", false, errors.New("sandbox CNI network name is required")
	}
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return "", false, fmt.Errorf("list CNI conf dir: %w", err)
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".conflist") || strings.HasSuffix(name, ".conf") {
			candidates = append(candidates, filepath.Join(confDir, name))
		}
	}
	sort.Strings(candidates)
	// Every candidate must parse, even ones past the match: a corrupt
	// file in the conf dir is operator error and fails the build
	// rather than being silently skipped.
	names := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			return "", false, fmt.Errorf("read CNI config %s: %w", candidate, err)
		}
		var decoded struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			return "", false, fmt.Errorf("parse CNI config %s: %w", candidate, err)
		}
		names[candidate] = decoded.Name
	}
	for _, candidate := range candidates {
		if names[candidate] == network {
			return candidate, strings.HasSuffix(candidate, ".conflist"), nil
		}
	}
	return "", false, fmt.Errorf("CNI network %q is not configured in %s", network, confDir)
}
