package volumestore

import (
	"os"
	"path/filepath"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func conditionFor(t *testing.T, conditions []*agentv1.VolumeCondition, id string) *agentv1.VolumeCondition {
	t.Helper()
	for _, cond := range conditions {
		if cond.GetVolumeId() == id {
			return cond
		}
	}
	t.Fatalf("no condition for volume %s in %v", id, conditions)
	return nil
}

func newDirectoryStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "volumes"), BackendDirectory)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeVolumeFile(t *testing.T, store *Store, id, name string, size int) string {
	t.Helper()
	dir, err := store.DataPath(id)
	if err != nil {
		t.Fatalf("DataPath(%s): %v", id, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMissingDesiredEntryRetainsDataAsOrphan(t *testing.T) {
	store := newDirectoryStore(t)
	volume := &agentv1.DesiredVolume{VolumeId: "vol-a", SizeBytes: 64 << 20}
	if cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-a"); cond.GetPhase() != PhaseReady {
		t.Fatalf("phase = %s (%s), want Ready", cond.GetPhase(), cond.GetMessage())
	}
	path := writeVolumeFile(t, store, "vol-a", "db", 1<<20)

	// A bad checkpoint or placement bug drops the entry; the data must survive.
	cond := conditionFor(t, store.Reconcile(nil, true), "vol-a")
	if cond.GetPhase() != PhaseOrphaned {
		t.Fatalf("phase = %s, want Orphaned", cond.GetPhase())
	}
	if cond.GetUsedBytes() < 1<<20 {
		t.Fatalf("orphan used bytes = %d, want >= 1MiB", cond.GetUsedBytes())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("volume data removed without a destroy instruction: %v", err)
	}

	// Restoring the entry reattaches the same data.
	store.Reconcile([]*agentv1.DesiredVolume{volume}, true)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("volume data lost after restore: %v", err)
	}
}

func TestDestroyIsFencedByRecovery(t *testing.T) {
	store := newDirectoryStore(t)
	store.Reconcile([]*agentv1.DesiredVolume{{VolumeId: "vol-a", SizeBytes: 64 << 20}}, true)
	path := writeVolumeFile(t, store, "vol-a", "db", 16)
	destroy := []*agentv1.DesiredVolume{{VolumeId: "vol-a", SizeBytes: 64 << 20, Destroy: true}}

	for _, cond := range store.Reconcile(destroy, false) {
		if cond.GetVolumeId() == "vol-a" {
			t.Fatalf("recovery-mode reconcile reported %s for a destroy instruction", cond.GetPhase())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recovery-mode reconcile destroyed data: %v", err)
	}

	if cond := conditionFor(t, store.Reconcile(destroy, true), "vol-a"); cond.GetPhase() != PhaseDestroyed {
		t.Fatalf("phase = %s (%s), want Destroyed", cond.GetPhase(), cond.GetMessage())
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("destroyed volume still on disk: %v", err)
	}
	// Destroy is idempotent once the data is gone.
	if cond := conditionFor(t, store.Reconcile(destroy, true), "vol-a"); cond.GetPhase() != PhaseDestroyed {
		t.Fatalf("repeat destroy phase = %s, want Destroyed", cond.GetPhase())
	}
}

func TestDirectoryBackendReportsFull(t *testing.T) {
	store := newDirectoryStore(t)
	volume := &agentv1.DesiredVolume{VolumeId: "vol-a", SizeBytes: 8 << 20}
	store.Reconcile([]*agentv1.DesiredVolume{volume}, true)
	writeVolumeFile(t, store, "vol-a", "big", 7<<20)
	cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-a")
	if cond.GetPhase() != PhaseFull {
		t.Fatalf("phase = %s, want Full", cond.GetPhase())
	}
	if cond.GetUsedBytes() != 7<<20 || cond.GetCapacityBytes() != 8<<20 {
		t.Fatalf("usage = %d/%d, want 7MiB/8MiB", cond.GetUsedBytes(), cond.GetCapacityBytes())
	}
	volume.SizeBytes = 64 << 20
	if cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-a"); cond.GetPhase() != PhaseReady {
		t.Fatalf("grown phase = %s, want Ready", cond.GetPhase())
	}
}

func TestDataPathRequiresProvisionedVolume(t *testing.T) {
	store := newDirectoryStore(t)
	if _, err := store.DataPath("vol-a"); err == nil {
		t.Fatal("DataPath succeeded for a volume that was never provisioned")
	}
	if _, err := store.DataPath("../escape"); err == nil {
		t.Fatal("DataPath accepted a path-escaping volume ID")
	}
}
