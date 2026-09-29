// Package builder executes builds with isolated production and local development executors.
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
	"ebof-wg-mesh/internal/config"
)

// ExecutorDevelopment runs builds as host children without isolation; production refuses it.
const ExecutorDevelopment = "development"

// ExecutorHardened runs every build step in a one-shot OCI sandbox with private namespaces and enforced egress.
const ExecutorHardened = "hardened"

var ErrBuildTimeout = errors.New("build timed out")

// BuildExecutor runs one customer build inside an explicitly described boundary
// and owns workspace lifecycle, verifying removal on completion and death.
type BuildExecutor interface {
	Name() string
	Isolating() bool
	// Execute runs a single build and returns the pushed image's digest-pinned reference.
	Execute(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error)
	RecoverStaleWorkspaces(ctx context.Context) (int, error)
}

// ExecutionSpec is the complete explicit input to one build execution.
type ExecutionSpec struct {
	BuildID string

	// SnapshotArchivePath is a compressed snapshot archive staged by the caller.
	SnapshotArchivePath string
	SnapshotDigest      string

	Recipe *platformv1.BuildRecipe

	Push     PushCredentials
	Buildkit BuildkitEndpoint
	Railpack RailpackToolchain

	Limits  ResourceLimits
	Network NetworkPolicy
	Cache   CachePolicy

	// Cleanup destroys and verifies workspace removal; disable only to debug a failed build.
	Cleanup bool

	OnLog func(commandOutputLine)
}

type ExecutionResult struct {
	// ImageDigestRef is the digest-pinned runtime reference (repository@digest).
	ImageDigestRef string
}

// PushCredentials are registry credentials scoped to exactly the repository named by Reference.
type PushCredentials struct {
	Reference string
	Username  string
	Password  string
}

// BuildkitEndpoint is the BuildKit endpoint for this execution. The hardened executor starts its own daemon.
type BuildkitEndpoint struct {
	Binary  string
	Address string
}

type RailpackToolchain struct {
	Binary        string
	FrontendImage string
}

// ResourceLimits are explicit per-execution limits; zero values are rejected by Validate.
type ResourceLimits struct {
	// Timeout bounds total execution time including snapshot setup.
	Timeout time.Duration
	// MemoryBytes caps child virtual address space (RLIMIT_AS), not resident set; it
	// needs headroom for Go toolchains. The hardened executor also enforces an RSS cap.
	MemoryBytes int64
	// CPUSeconds caps child-process CPU time (RLIMIT_CPU).
	CPUSeconds int64
	// MaxFileBytes caps any single file a build child writes (RLIMIT_FSIZE).
	MaxFileBytes int64
	// MaxProcesses caps the number of processes a build child may fork (RLIMIT_NPROC).
	MaxProcesses int64
	// MaxWorkspaceBytes caps total build-attributable bytes, accounted after the build.
	MaxWorkspaceBytes int64
}

func DefaultResourceLimits() ResourceLimits {
	limits := config.DefaultBuilderLimits()
	return ResourceLimits{
		Timeout:           time.Duration(limits.TimeoutSeconds) * time.Second,
		MemoryBytes:       limits.MemoryBytes,
		CPUSeconds:        limits.CPUSeconds,
		MaxFileBytes:      limits.MaxFileBytes,
		MaxProcesses:      limits.MaxProcesses,
		MaxWorkspaceBytes: limits.MaxWorkspaceBytes,
	}
}

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

// ProcessLimits is the subset of ResourceLimits applied to spawned build children.
type ProcessLimits struct {
	MemoryBytes  int64
	CPUSeconds   int64
	MaxFileBytes int64
	MaxProcesses int64
}

func (l ResourceLimits) ProcessLimits() ProcessLimits {
	return ProcessLimits{
		MemoryBytes:  l.MemoryBytes,
		CPUSeconds:   l.CPUSeconds,
		MaxFileBytes: l.MaxFileBytes,
		MaxProcesses: l.MaxProcesses,
	}
}

// NetworkPolicy is the restricted network policy for one build execution:
// loopback-only or CNI-attached with denied-CIDR blackholes.
type NetworkPolicy struct {
	AllowGeneralEgress bool
	DeniedCIDRs        []string
}

// DefaultRestrictedNetworkPolicy blocks cloud metadata endpoints.
func DefaultRestrictedNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{
		AllowGeneralEgress: true,
		DeniedCIDRs:        config.DefaultBuilderDeniedCIDRs(),
	}
}

func (p NetworkPolicy) Validate() error {
	for _, raw := range p.DeniedCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("denied egress CIDR %q: %w", raw, err)
		}
	}
	return nil
}

type CacheMode string

const (
	CacheModeNone CacheMode = "none"
	// CacheModeContentAddressed persists cache only under content-derived keys.
	CacheModeContentAddressed CacheMode = "content-addressed"
)

type CachePolicy struct {
	Mode CacheMode
	Key  string
}

// Validate requires an explicit mode and, for content-addressed mode, a key.
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

// ContentCacheKey derives a cache key purely from build content, so cached data
// cannot carry state between projects.
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
