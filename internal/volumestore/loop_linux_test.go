//go:build linux

package volumestore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func requireLoopHost(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loop volumes require root")
	}
	for _, tool := range []string{"mkfs.ext4", "resize2fs", "losetup", "mount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

func TestLoopVolumeEnforcesSizeAndGrowsInPlace(t *testing.T) {
	requireLoopHost(t)
	store, err := New(filepath.Join(t.TempDir(), "volumes"), BackendLoop)
	if err != nil {
		t.Fatal(err)
	}
	volume := &agentv1.DesiredVolume{VolumeId: "vol-loop", SizeBytes: 32 << 20}
	t.Cleanup(func() {
		store.Reconcile([]*agentv1.DesiredVolume{{VolumeId: "vol-loop", Destroy: true}}, true)
	})
	if cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-loop"); cond.GetPhase() != PhaseReady {
		t.Fatalf("phase = %s (%s), want Ready", cond.GetPhase(), cond.GetMessage())
	}
	dir, err := store.DataPath("vol-loop")
	if err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, []byte("survives"), 0o644); err != nil {
		t.Fatal(err)
	}

	fill, err := os.Create(filepath.Join(dir, "fill"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	var writeErr error
	for i := 0; i < 64 && writeErr == nil; i++ {
		_, writeErr = fill.Write(chunk)
		if writeErr == nil {
			writeErr = fill.Sync()
		}
	}
	fill.Close()
	if !errors.Is(writeErr, syscall.ENOSPC) {
		t.Fatalf("writing past the volume size returned %v, want ENOSPC", writeErr)
	}
	if cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-loop"); cond.GetPhase() != PhaseFull {
		t.Fatalf("phase = %s, want Full", cond.GetPhase())
	}

	volume.SizeBytes = 96 << 20
	cond := conditionFor(t, store.Reconcile([]*agentv1.DesiredVolume{volume}, true), "vol-loop")
	if cond.GetPhase() != PhaseReady {
		t.Fatalf("grown phase = %s (%s), want Ready", cond.GetPhase(), cond.GetMessage())
	}
	if cond.GetCapacityBytes() <= 64<<20 {
		t.Fatalf("grown capacity = %d, want > 64MiB", cond.GetCapacityBytes())
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "survives" {
		t.Fatalf("data after grow = %q, %v", got, err)
	}

	// Missing from desired state: still mounted, still there.
	if cond := conditionFor(t, store.Reconcile(nil, true), "vol-loop"); cond.GetPhase() != PhaseOrphaned {
		t.Fatalf("phase = %s, want Orphaned", cond.GetPhase())
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("orphaned loop volume lost data: %v", err)
	}
}
