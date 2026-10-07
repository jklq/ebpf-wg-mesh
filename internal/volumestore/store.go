// Package volumestore owns node-local volume data for an agent runtime.
//
// A volume is never deleted because it is missing from desired state. Unknown
// volume directories are retained and reported as orphaned; only an explicit
// destroy instruction removes data.
package volumestore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

// Backend selects how volume size is enforced.
type Backend string

const (
	// BackendLoop formats each volume as an ext4 image on a loop device, so a
	// full volume returns ENOSPC to the workload without filling the host disk.
	BackendLoop Backend = "loop"
	// BackendDirectory stores plain directories. Size is reported, not
	// enforced; it exists for development hosts that cannot loop-mount.
	BackendDirectory Backend = "directory"
)

const (
	PhaseReady     = "Ready"
	PhaseFull      = "Full"
	PhaseError     = "Error"
	PhaseDestroyed = "Destroyed"
	PhaseOrphaned  = "Orphaned"
)

// usage granularity keeps status reports from changing on every write.
const usageGranularity = 1 << 20

// A volume is full once free space drops below the larger of these.
const (
	fullFloorBytes = 4 << 20
	fullFraction   = 200
)

var ErrNotProvisioned = errors.New("volume is not provisioned on this node")

type backend interface {
	// ensure provisions the volume or grows it to sizeBytes and makes its data
	// directory available. It never shrinks or reformats existing data.
	ensure(root string, sizeBytes int64) error
	// dataPath is the directory bound into workloads. It fails if the volume's
	// storage is not provisioned and mounted, so a workload never writes to an
	// unenforced host directory in its place.
	dataPath(root string) (string, error)
	usage(root string, sizeBytes int64) (used, capacity, available int64, err error)
	destroy(root string) error
}

// Store manages volumes under one directory: <dir>/<volume-id>/.
type Store struct {
	dir     string
	backend backend
	kind    Backend
}

func New(dir string, kind Backend) (*Store, error) {
	var impl backend
	switch kind {
	case BackendLoop:
		impl = loopBackend{}
	case BackendDirectory:
		impl = directoryBackend{}
	default:
		return nil, fmt.Errorf("unknown volume backend %q", kind)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir volumes dir: %w", err)
	}
	return &Store{dir: dir, backend: impl, kind: kind}, nil
}

func (s *Store) Backend() Backend { return s.kind }

// Reconcile provisions retained volumes, destroys volumes whose destroy
// instruction was accepted, and reports everything else on disk as orphaned.
// Destruction is fenced by allowDestroy: an agent in local-state recovery
// cannot prove the instruction is current and must not delete data.
func (s *Store) Reconcile(desired []*agentv1.DesiredVolume, allowDestroy bool) []*agentv1.VolumeCondition {
	known := make(map[string]bool, len(desired))
	var out []*agentv1.VolumeCondition
	for _, volume := range desired {
		id := volume.GetVolumeId()
		known[id] = true
		if volume.GetDestroy() {
			if !allowDestroy {
				continue
			}
			out = append(out, s.destroy(id))
			continue
		}
		out = append(out, s.ensure(volume))
	}
	ids, err := s.onDisk()
	if err != nil {
		return append(out, &agentv1.VolumeCondition{Phase: PhaseError, Message: err.Error()})
	}
	for _, id := range ids {
		if known[id] {
			continue
		}
		cond := &agentv1.VolumeCondition{VolumeId: id, Phase: PhaseOrphaned,
			Message: "volume data has no desired entry on this node; retained until an explicit destroy"}
		if root, err := s.root(id); err == nil {
			if used, capacity, _, err := s.backend.usage(root, 0); err == nil {
				cond.UsedBytes, cond.CapacityBytes = roundUsage(used), capacity
			}
		}
		out = append(out, cond)
	}
	return out
}

// DataPath returns the directory to bind into a workload for volumeID.
func (s *Store) DataPath(volumeID string) (string, error) {
	root, err := s.root(volumeID)
	if err != nil {
		return "", err
	}
	return s.backend.dataPath(root)
}

func (s *Store) ensure(volume *agentv1.DesiredVolume) *agentv1.VolumeCondition {
	id := volume.GetVolumeId()
	cond := &agentv1.VolumeCondition{VolumeId: id}
	fail := func(err error) *agentv1.VolumeCondition {
		cond.Phase, cond.Message = PhaseError, err.Error()
		return cond
	}
	root, err := s.root(id)
	if err != nil {
		return fail(err)
	}
	if volume.GetSizeBytes() <= 0 {
		return fail(fmt.Errorf("volume %s has no size", id))
	}
	if err := s.backend.ensure(root, volume.GetSizeBytes()); err != nil {
		return fail(err)
	}
	used, capacity, available, err := s.backend.usage(root, volume.GetSizeBytes())
	if err != nil {
		return fail(err)
	}
	cond.UsedBytes, cond.CapacityBytes = roundUsage(used), capacity
	cond.Phase = PhaseReady
	if full(capacity, available) {
		cond.Phase = PhaseFull
		cond.Message = "volume is full; writes fail with ENOSPC until it is grown"
	}
	return cond
}

func (s *Store) destroy(id string) *agentv1.VolumeCondition {
	root, err := s.root(id)
	if err != nil {
		return &agentv1.VolumeCondition{VolumeId: id, Phase: PhaseError, Message: err.Error()}
	}
	if _, err := os.Lstat(root); err == nil {
		if err := s.backend.destroy(root); err != nil {
			return &agentv1.VolumeCondition{VolumeId: id, Phase: PhaseError, Message: fmt.Sprintf("destroy volume: %v", err)}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return &agentv1.VolumeCondition{VolumeId: id, Phase: PhaseError, Message: err.Error()}
	}
	return &agentv1.VolumeCondition{VolumeId: id, Phase: PhaseDestroyed}
}

func (s *Store) onDisk() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read volumes dir: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && validID(entry.Name()) == nil {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) root(id string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	base, err := filepath.Abs(filepath.Clean(s.dir))
	if err != nil {
		return "", fmt.Errorf("resolve volumes dir: %w", err)
	}
	return filepath.Join(base, id), nil
}

func validID(id string) error {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return fmt.Errorf("invalid volume ID %q", id)
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return fmt.Errorf("invalid volume ID %q", id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid volume ID %q", id)
	}
	return nil
}

func full(capacity, available int64) bool {
	if capacity <= 0 {
		return false
	}
	return available < max(int64(fullFloorBytes), capacity/fullFraction)
}

func roundUsage(used int64) int64 {
	if used <= 0 {
		return 0
	}
	return (used + usageGranularity - 1) / usageGranularity * usageGranularity
}

// Inventory lists durable volumes without ensuring, mounting, resizing or deleting them.
func (s *Store) Inventory() ([]string, error) { return s.onDisk() }
