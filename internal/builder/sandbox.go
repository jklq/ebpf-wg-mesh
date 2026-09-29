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

// sandboxBackendName is the only sandbox backend; unknown backends fail validation.
const sandboxBackendName = "containerd"

// sandboxBuildRoot is the guest path of the execution workspace in every sandbox.
const sandboxBuildRoot = "/build"

// SandboxBackend runs build steps inside one-shot sandboxes: containment
// primitives only, plus reaping its own crashed state.
type SandboxBackend interface {
	// SetupNet creates the execution network: loopback-only or CNI-attached with
	// denied-CIDR blackholes. It never falls back to the host network.
	SetupNet(ctx context.Context, buildID string, policy NetworkPolicy) (SandboxNet, error)
	// RunStep runs one command in a fresh one-shot sandbox attached to net.
	RunStep(ctx context.Context, net SandboxNet, step SandboxStep) error
	// TeardownNet removes execution network state. Best effort; reports joined errors.
	TeardownNet(ctx context.Context, net SandboxNet) error
	// ReapStale removes sandboxes and networks left behind by dead builders.
	ReapStale(ctx context.Context) (int, error)
	Close() error
}

// SandboxBackendConfig carries the backend's explicit inputs.
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

type SandboxNet struct {
	BuildID string
	Path    string
	// Attached reports whether the namespace is CNI-attached or loopback-only.
	Attached bool
}

type SandboxStep struct {
	Name string
	Argv []string
	// Env is the complete explicit environment; the backend never inherits the host's.
	Env    []string
	Dir    string
	Limits ResourceLimits
	// Mounts are host-to-guest bind mounts confined to the build root and resolver files.
	Mounts []SandboxMount
	OnLog  func(commandOutputLine)
}

type SandboxMount struct {
	Source   string
	Dest     string
	ReadOnly bool
}

// SandboxStepError reports a sandboxed step that exited nonzero, with bounded tail output.
type SandboxStepError struct {
	Step     string
	ExitCode int
	Tail     string
}

func (e *SandboxStepError) Error() string {
	return fmt.Sprintf("sandbox step %q exited with code %d", e.Step, e.ExitCode)
}

// NewSandboxBackend opens the configured sandbox backend. Only containerd exists.
func NewSandboxBackend(cfg SandboxBackendConfig) (SandboxBackend, error) {
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("sandbox image is required for the hardened executor")
	}
	return newSandboxBackendPlatform(cfg)
}

// validateSandboxMounts rejects mounts exposing host state beyond the workspace
// and resolver files. Sources must be absolute and exist.
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

// guestWorkspacePath translates a host path under the workspace root to its guest path.
func guestWorkspacePath(workspaceRoot, hostPath string) (string, error) {
	root, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", err
	}
	target := ""
	for prefix := filepath.Clean(hostPath); ; prefix = filepath.Dir(prefix) {
		resolved, resolveErr := filepath.EvalSymlinks(prefix)
		if resolveErr == nil {
			suffix, relErr := filepath.Rel(prefix, hostPath)
			if relErr != nil {
				return "", relErr
			}
			target = filepath.Join(resolved, suffix)
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) || prefix == filepath.Dir(prefix) {
			return "", resolveErr
		}
	}
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

// sandboxContainerID derives a containerd-safe container ID from a build ID and step name.
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

// deniedRoutePrefixes parses the denied CIDRs into route targets. Invalid CIDRs fail the build.
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

// renderSandboxResolvConf renders the resolver configuration for sandboxes.
// Explicit nameservers win; otherwise host resolvers are inherited with loopback
// entries dropped. Loopback-only inheritance with general egress fails closed.
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

// renderSandboxHostsFile renders the minimal hosts file: loopback and sandbox hostname only.
func renderSandboxHostsFile() []byte {
	return []byte("127.0.0.1 localhost\n::1 localhost\n127.0.0.1 build\n::1 build\n")
}

// selectCNIConfig finds the CNI configuration file for the named network by exact
// name, never directory order.
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
	// Every candidate must parse, even past the match: a corrupt file fails the build.
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
