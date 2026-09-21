// Package builder executes customer builds behind the BuildExecutor
// boundary (2.4a).
//
// Every build goes through a BuildExecutor. Each execution receives its
// inputs explicitly: a verified source snapshot archive, an isolated
// writable workspace, a BuildKit endpoint, push credentials scoped to
// exactly one repository, resource limits, and a network policy. The
// executor owns its workspace lifecycle and verifies cleanup on
// completion, cancellation, and worker death (via RecoverStaleWorkspaces
// on the next start).
//
// The development executor establishes the seam, the limits, and the
// credential scoping that the hardened backend enforces; it does not
// isolate hostile code and is labeled non-isolating in the builder
// startup contract. The hardened executor is the production backend.
package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

// ExecutorDevelopment runs builds as host child processes. It does not
// isolate hostile code; production refuses it at config validation
// time.
const ExecutorDevelopment = "development"

// ExecutorHardened runs every build step inside a one-shot OCI sandbox
// with its own mount, PID, network, IPC, UTS, and cgroup namespaces,
// a private network namespace with enforced egress policy, and a
// per-execution BuildKit daemon. It is the production backend.
const ExecutorHardened = "hardened"

// ErrBuildTimeout reports a build that exceeded its executor time limit.
var ErrBuildTimeout = errors.New("build timed out")

// BuildExecutor runs one customer build inside an explicitly described
// boundary. Implementations own workspace lifecycle: they destroy the
// execution workspace and verify its removal on completion,
// cancellation, and (via RecoverStaleWorkspaces after a restart)
// worker death.
type BuildExecutor interface {
	// Name identifies the executor selection, e.g. "development".
	Name() string
	// Isolating reports whether the executor holds against hostile
	// code. The development executor returns false.
	Isolating() bool
	// Execute runs a single build to completion and returns the
	// digest-pinned runtime reference of the pushed image.
	Execute(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error)
	// RecoverStaleWorkspaces removes workspaces left behind by dead
	// workers and verifies each removal. It returns the number of
	// workspaces reclaimed.
	RecoverStaleWorkspaces(ctx context.Context) (int, error)
}

// ExecutionSpec is the complete explicit input to one build execution.
// Nothing about the source, credentials, endpoint, limits, or policy
// may be read from ambient host state.
type ExecutionSpec struct {
	BuildID       string
	ServiceID     string
	ProjectID     string
	EnvironmentID string
	CommitSHA     string

	// SnapshotArchivePath is a verified compressed snapshot archive
	// (see 2.2) staged by the caller. The executor extracts it into a
	// read-only snapshot directory and never writes into that directory.
	SnapshotArchivePath string
	SnapshotID          string
	SnapshotDigest      string

	Recipe *platformv1.BuildRecipe

	Push     PushCredentials
	Buildkit BuildkitEndpoint
	Railpack RailpackToolchain

	Limits  ResourceLimits
	Network NetworkPolicy
	Cache   CachePolicy

	// Cleanup destroys and verifies workspace removal when true.
	// Disable only to debug a failed build.
	Cleanup bool

	// OnLog receives streamed build output lines.
	OnLog func(commandOutputLine)
}

// ExecutionResult is the outcome of one build execution.
type ExecutionResult struct {
	// ImageDigestRef is the digest-pinned runtime reference of the
	// pushed image (repository@digest).
	ImageDigestRef string
}

// PushCredentials are registry credentials scoped to exactly the one
// repository named by Reference. The executor writes them into a
// per-execution docker config containing no other credential and
// refuses to run when they are missing or do not match Reference.
type PushCredentials struct {
	Reference string
	Username  string
	Password  string
}

// BuildkitEndpoint is the BuildKit endpoint isolated to this execution.
// The development executor receives the single configured endpoint
// explicitly rather than reading it from ambient state; per-execution
// isolation of the daemon itself is 2.4b work.
type BuildkitEndpoint struct {
	Binary  string
	Address string
}

// RailpackToolchain locates the plan binary and frontend image for
// railpack builds.
type RailpackToolchain struct {
	Binary        string
	FrontendImage string
}

// ResourceLimits are explicit per-execution CPU/memory/disk/PID/time
// limits. Zero values mean unset and are rejected by Validate; the
// caller supplies effective defaults from builder configuration.
type ResourceLimits struct {
	// Timeout bounds total execution time including snapshot setup.
	Timeout time.Duration
	// MemoryBytes caps child-process virtual address space
	// (RLIMIT_AS). Virtual, not resident: Go-based build tools
	// reserve over a gigabyte at startup, so this needs generous
	// headroom and bounds runaway reservation rather than
	// containing RSS. Hard resident-set containment is 2.4b work.
	MemoryBytes int64
	// CPUSeconds caps child-process CPU time (RLIMIT_CPU).
	CPUSeconds int64
	// MaxFileBytes caps any single file a build child writes
	// (RLIMIT_FSIZE).
	MaxFileBytes int64
	// MaxProcesses caps the number of processes a build child may
	// fork (RLIMIT_NPROC).
	MaxProcesses int64
	// MaxWorkspaceBytes caps total executor-managed workspace bytes,
	// accounted after the build.
	MaxWorkspaceBytes int64
}

// DefaultResourceLimits returns the effective defaults used when the
// operator configures no explicit limits.
func DefaultResourceLimits() ResourceLimits {
	return ResourceLimits{
		Timeout:           30 * time.Minute,
		MemoryBytes:       8 << 30,
		CPUSeconds:        3600,
		MaxFileBytes:      10 << 30,
		MaxProcesses:      4096,
		MaxWorkspaceBytes: 20 << 30,
	}
}

// Validate rejects unset or nonsensical limits.
func (l ResourceLimits) Validate() error {
	if l.Timeout <= 0 {
		return errors.New("build timeout must be greater than 0")
	}
	if l.MemoryBytes <= 0 {
		return errors.New("build memory limit must be greater than 0")
	}
	if l.CPUSeconds <= 0 {
		return errors.New("build CPU limit must be greater than 0")
	}
	if l.MaxFileBytes <= 0 {
		return errors.New("build file size limit must be greater than 0")
	}
	if l.MaxProcesses <= 0 {
		return errors.New("build process limit must be greater than 0")
	}
	if l.MaxWorkspaceBytes <= 0 {
		return errors.New("build workspace limit must be greater than 0")
	}
	return nil
}

// ProcessLimits is the subset of ResourceLimits applied to spawned
// build child processes.
type ProcessLimits struct {
	MemoryBytes  int64
	CPUSeconds   int64
	MaxFileBytes int64
	MaxProcesses int64
}

// ProcessLimits derives the child-process limits from the execution
// limits.
func (l ResourceLimits) ProcessLimits() ProcessLimits {
	return ProcessLimits{
		MemoryBytes:  l.MemoryBytes,
		CPUSeconds:   l.CPUSeconds,
		MaxFileBytes: l.MaxFileBytes,
		MaxProcesses: l.MaxProcesses,
	}
}

// NetworkPolicy is the restricted network policy for one build
// execution, expressed as input rather than ambient host state. The
// development executor validates the policy, scrubs ambient
// network-shaping environment (proxy variables, docker contexts) from
// build children, and honestly reports itself non-isolating; CIDR
// enforcement at the build data plane is 2.4b work.
type NetworkPolicy struct {
	AllowGeneralEgress bool
	DeniedCIDRs        []string
}

// DefaultRestrictedNetworkPolicy denies the platform's own surface
// (cloud metadata endpoints) while allowing general egress for
// dependency fetches during the build.
func DefaultRestrictedNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{
		AllowGeneralEgress: true,
		DeniedCIDRs: []string{
			"169.254.169.254/32",
			"100.100.100.200/32",
			"fd00:ec2::254/128",
		},
	}
}

// Validate parses every denied CIDR.
func (p NetworkPolicy) Validate() error {
	for _, raw := range p.DeniedCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("denied egress CIDR %q: %w", raw, err)
		}
	}
	return nil
}

// CacheMode selects how an executor may persist build cache data.
type CacheMode string

const (
	// CacheModeNone persists no cache between executions.
	CacheModeNone CacheMode = "none"
	// CacheModeContentAddressed persists cache only under keys
	// derived from build content (see ContentCacheKey), never under
	// project, service, or build identity.
	CacheModeContentAddressed CacheMode = "content-addressed"
)

// CachePolicy carries the cache mode and, for content-addressed mode,
// the execution's cache key.
type CachePolicy struct {
	Mode CacheMode
	Key  string
}

// Validate requires an explicit mode and, for content-addressed mode,
// a key.
func (p CachePolicy) Validate() error {
	switch p.Mode {
	case CacheModeNone:
		return nil
	case CacheModeContentAddressed:
		if strings.TrimSpace(p.Key) == "" {
			return errors.New("content-addressed cache requires a key")
		}
		return nil
	default:
		return fmt.Errorf("unknown build cache mode %q", p.Mode)
	}
}

// ContentCacheKey derives a cache key purely from build content —
// source snapshot digest, recipe, and toolchain — so cached data
// cannot carry state between projects. Project, service, environment,
// and build identity are deliberately excluded: identical content
// shares entries safely because the key is the content.
func ContentCacheKey(snapshotDigest string, recipe *platformv1.BuildRecipe, frontendImage string) string {
	var parts []string
	parts = append(parts, "snapshot="+strings.TrimSpace(snapshotDigest))
	if recipe != nil {
		parts = append(parts, fmt.Sprintf("builder=%d", recipe.GetBuilder()))
		parts = append(parts, "context="+recipe.GetContextDir())
		parts = append(parts, "dockerfile="+recipe.GetDockerfilePath())
	}
	parts = append(parts, "frontend="+strings.TrimSpace(frontendImage))
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
