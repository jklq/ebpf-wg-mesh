// Package isolation is the tenant-isolation module for untrusted workloads
// and builds. Its interface is small on purpose: a caller picks a Class (or
// accepts the profile default), asks for the containerd runtime name, the OCI
// identity mappings for an isolation scope, and host-ID translations for
// filesystem ownership. Everything else — which kernel boundary a class
// provides, how host UID ranges are allocated, which host binaries must exist
// — lives behind that seam.
//
// Plain runc on the host kernel with host root is deliberately not a class:
// there is no configuration value that runs tenant code as host root. A class
// whose host prerequisites are missing fails closed (VerifyHost); nothing in
// this package ever substitutes a weaker class.
package isolation

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Class names the kernel boundary tenant code runs behind.
type Class string

const (
	// ClassRuncUserNS runs runc containers in a dedicated ID-mapped user
	// namespace per isolation scope: root in the container is an unprivileged
	// host UID, and capabilities gained inside have no effect outside. The
	// host kernel is still shared, so this is the floor, not the goal.
	ClassRuncUserNS Class = "runc-userns"
	// ClassGVisor runs containers under gVisor (runsc): tenant syscalls are
	// intercepted by a userspace kernel (the Sentry) instead of reaching the
	// host kernel directly.
	ClassGVisor Class = "gvisor"
	// ClassKata runs containers inside a lightweight VM (Kata Containers):
	// tenant code gets its own guest kernel.
	ClassKata Class = "kata"
)

// User-namespace host range layout for ClassRuncUserNS. Each isolation scope
// (a workload environment, or the builder) deterministically owns one
// 64K-ID slot out of usernsSlots, starting at usernsBase. The range is far
// above any plausible host account allocation and far below the overflow ID.
const (
	usernsBase  uint32 = 100000
	usernsSize  uint32 = 65536
	usernsSlots uint32 = 4096
)

// NormalizeClass resolves an operator-supplied class value. Empty selects the
// profile default: kernel separation (gVisor) in production, ID-mapped user
// namespaces in development. Unknown values are errors, never a fallback.
func NormalizeClass(raw string, production bool) (Class, error) {
	switch Class(strings.TrimSpace(raw)) {
	case "":
		if production {
			return ClassGVisor, nil
		}
		return ClassRuncUserNS, nil
	case ClassRuncUserNS, ClassGVisor, ClassKata:
		return Class(strings.TrimSpace(raw)), nil
	default:
		return "", fmt.Errorf("isolation class %q is not supported: use %q, %q, or %q (plain host-root runc is not available for tenant code)", raw, ClassRuncUserNS, ClassGVisor, ClassKata)
	}
}

// Policy is a resolved isolation class plus its derived host facts. The zero
// value is invalid; use NewPolicy.
type Policy struct {
	class Class
}

// NewPolicy validates class and returns its policy.
func NewPolicy(class Class) (Policy, error) {
	switch class {
	case ClassRuncUserNS, ClassGVisor, ClassKata:
		return Policy{class: class}, nil
	default:
		return Policy{}, fmt.Errorf("isolation class %q is not supported", class)
	}
}

// PolicyFor resolves raw (with profile defaulting) into a Policy.
func PolicyFor(raw string, production bool) (Policy, error) {
	class, err := NormalizeClass(raw, production)
	if err != nil {
		return Policy{}, err
	}
	return Policy{class: class}, nil
}

func (p Policy) Class() Class { return p.class }

func (p Policy) String() string { return string(p.class) }

// ContainerdRuntime is the containerd runtime name containers of this class
// must be created with, explicitly — never the daemon default.
func (p Policy) ContainerdRuntime() string {
	switch p.class {
	case ClassGVisor:
		return "io.containerd.runsc.v1"
	case ClassKata:
		return "io.containerd.kata.v2"
	default:
		return "io.containerd.runc.v2"
	}
}

// ShimBinary is the containerd shim executable the host must provide for
// this class. Containerd discovers shims on PATH at container creation, so
// presence is verified up front instead of failing the first tenant.
func (p Policy) ShimBinary() string {
	switch p.class {
	case ClassGVisor:
		return "containerd-shim-runsc-v1"
	case ClassKata:
		return "containerd-shim-kata-v2"
	default:
		return "containerd-shim-runc-v2"
	}
}

// WorkerBinary is the OCI runtime executable a BuildKit OCI worker must use
// for build RUN steps of this class.
func (p Policy) WorkerBinary() string {
	switch p.class {
	case ClassGVisor:
		return "runsc"
	case ClassKata:
		return "kata-runtime"
	default:
		return "runc"
	}
}

// RunscConfig returns the runsc shim configuration (TOML) for
// kernel-separated gVisor containers, or false for other classes. The
// Sentry runs with host networking *inside the workload's own network
// namespace*: its netstack ("sandbox") mode does not complete link-layer
// setup against CNI-provisioned namespaces on current kernels (ARP never
// answers, verified live), while host mode sockets still egress through
// the workload's veth and mesh policy. Passing this via containerd
// runtime options (type URL io.containerd.runsc.v1.options, ConfigPath)
// is the shim's supported configuration seam.
func (p Policy) RunscConfig() (string, bool) {
	if p.class != ClassGVisor {
		return "", false
	}
	return "[runsc_config]\nnetwork=\"host\"\n", true
}

// RunscOptionsTypeURL is the containerd runtime-options type URL the
// gVisor shim accepts for ConfigPath-based configuration.
const RunscOptionsTypeURL = "io.containerd.runsc.v1.options"

// BuildKitWorkerBinary is the binary a BuildKit OCI worker must use for
// Dockerfile RUN steps of this class. Kata's runtime is a drop-in runc CLI.
// gVisor's runsc is not (different global flags), so builds go through the
// platform's runsc-buildkit translation binary, which execs runsc.
func (p Policy) BuildKitWorkerBinary() string {
	switch p.class {
	case ClassGVisor:
		return "runsc-buildkit"
	case ClassKata:
		return "kata-runtime"
	default:
		return "runc"
	}
}

// VerifySnapshotter checks the containerd snapshotter can present an
// ID-mapped root filesystem for user-namespace classes. Without idmapped
// mounts every scope would need its own chowned copy of every image layer,
// so this is a hard requirement, checked at startup from the daemon's
// advertised capabilities.
func (p Policy) VerifySnapshotter(name string, capabilities []string) error {
	if !p.UsesUserNamespaces() {
		return nil
	}
	for _, capability := range capabilities {
		if capability == "remap-ids" {
			return nil
		}
	}
	return fmt.Errorf("isolation class %q requires a snapshotter with idmapped-mount (remap-ids) support: snapshotter %q advertises %v; use overlayfs on a kernel with idmapped mounts (5.19+) or select a kernel-separated isolation class", p.class, name, capabilities)
}

// KernelSeparated reports whether tenant code is separated from the host
// kernel (userspace kernel or guest kernel) rather than merely de-privileged
// on it.
func (p Policy) KernelSeparated() bool {
	return p.class == ClassGVisor || p.class == ClassKata
}

// GVisorMinMemoryMebibytes is the smallest container memory limit a
// gVisor-class service can run with: the Sentry shares the container's
// memory limit (the shim passes it as runsc's --total-memory), so below
// roughly this floor the Sentry starves before the workload starts and the
// container hangs or crash-loops instead of failing cleanly.
const GVisorMinMemoryMebibytes int64 = 256

// MinMemoryMebibytes is the smallest service memory limit this class can
// run, or 0 when the class imposes no floor of its own.
func (p Policy) MinMemoryMebibytes() int64 {
	if p.class == ClassGVisor {
		return GVisorMinMemoryMebibytes
	}
	return 0
}

// ValidateMemoryMebibytes rejects a service memory limit this class cannot
// run. Undersized gVisor services are rejected, never silently inflated:
// the tenant asked for (and is billed for) the limit in the spec.
func (p Policy) ValidateMemoryMebibytes(mebibytes int64) error {
	if floor := p.MinMemoryMebibytes(); floor > 0 && mebibytes < floor {
		return fmt.Errorf("service memory_mebibytes %d is below the %d MiB minimum for isolation class %q: the gVisor Sentry shares the container memory limit and starves below it; increase memory_mebibytes to at least %d", mebibytes, floor, p.class, floor)
	}
	return nil
}

// RuntimeCapability is the agent runtime-capability token advertising a
// class to the controlplane, so placement can keep services the class
// cannot run off hosts that provide it.
func RuntimeCapability(class Class) string {
	return "isolation:" + string(class)
}

// RuntimeCapability is RuntimeCapability for the policy's class.
func (p Policy) RuntimeCapability() string {
	return RuntimeCapability(p.class)
}

// UsesUserNamespaces reports whether containers of this class run in an
// ID-mapped user namespace whose mappings this package owns. Kernel-separated
// classes do not need OCI identity mappings to keep container root off the
// host: guest/Sentry root is not a host identity at all.
func (p Policy) UsesUserNamespaces() bool {
	return p.class == ClassRuncUserNS
}

// Mappings returns the OCI UID and GID mappings for one isolation scope for
// user-namespace classes, and nil for kernel-separated classes. The scope is
// the platform isolation scope that owns persistent files (a workload
// environment ID): allocations come and go, but volume ownership must stay
// stable, and two scopes must not share a host identity range. Allocation is
// a pure function of the scope, so it survives restarts with no state.
func (p Policy) Mappings(scope string) ([]specs.LinuxIDMapping, []specs.LinuxIDMapping, error) {
	if !p.UsesUserNamespaces() {
		return nil, nil, nil
	}
	hostID, err := p.hostRoot(scope)
	if err != nil {
		return nil, nil, err
	}
	mapping := []specs.LinuxIDMapping{{ContainerID: 0, HostID: hostID, Size: usernsSize}}
	return mapping, append([]specs.LinuxIDMapping(nil), mapping...), nil
}

// HostUID translates a container UID to the host UID that owns its files on
// disk for the scope. Kernel-separated classes have no mapping: files are
// owned by the container UID directly (bind mounts cross no user namespace).
func (p Policy) HostUID(scope string, containerUID uint32) (uint32, error) {
	return p.hostID(scope, containerUID)
}

// HostGID is HostUID for group IDs; UID and GID ranges are allocated
// identically so a scope's files stay self-consistent.
func (p Policy) HostGID(scope string, containerGID uint32) (uint32, error) {
	return p.hostID(scope, containerGID)
}

func (p Policy) hostID(scope string, containerID uint32) (uint32, error) {
	if !p.UsesUserNamespaces() {
		return containerID, nil
	}
	if containerID >= usernsSize {
		return 0, fmt.Errorf("container ID %d is outside the %d-ID user namespace range for scope %q", containerID, usernsSize, scope)
	}
	root, err := p.hostRoot(scope)
	if err != nil {
		return 0, err
	}
	return root + containerID, nil
}

func (p Policy) hostRoot(scope string) (uint32, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return 0, errors.New("isolation scope is required to allocate a user namespace range")
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(scope))
	return usernsBase + (hash.Sum32()%usernsSlots)*usernsSize, nil
}
