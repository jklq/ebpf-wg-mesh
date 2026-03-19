package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

type fakeEngine struct {
	ensured []string
	removed []string
	status  map[string]serviceStatus
	created map[string]bool
}

func (f *fakeEngine) EnsureService(_ context.Context, svc *agentv1.DesiredService) (serviceStatus, bool, error) {
	f.ensured = append(f.ensured, svc.GetAllocationId())
	return f.status[svc.GetAllocationId()], f.created[svc.GetAllocationId()], nil
}

func (f *fakeEngine) RemoveService(_ context.Context, allocationID string) error {
	f.removed = append(f.removed, allocationID)
	return nil
}

func (f *fakeEngine) Close() error { return nil }

func TestContainerdRuntimeReconcilePersistsDesiredStateAndCallsEngine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{
		status: map[string]serviceStatus{"alloc-1": {
			AppliedSpecRevision:      2,
			AppliedRolloutGeneration: 2,
			Endpoint:                 "fd00::10:8080",
		}},
		created: map[string]bool{"alloc-1": true},
	}
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{
			Runtime: config.RuntimeConfig{
				DataDir:    dir,
				VolumesDir: filepath.Join(dir, "volumes"),
			},
		},
		engine: engine,
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := &agentv1.DesiredNodeState{
		AgentId:  "node-1",
		Revision: 2,
		Volumes:  []*agentv1.DesiredVolume{{VolumeId: "vol-1", Name: "data"}},
		Services: []*agentv1.DesiredService{{
			AllocationId:             "alloc-1",
			ServiceId:                "svc-1",
			DesiredSpecRevision:      2,
			DesiredRolloutGeneration: 2,
			PrivateIpv6:              "fd00::10",
			Spec:                     &platformv1.ServiceSpec{ContainerPort: 8080},
		}},
	}
	report, err := runtime.Reconcile(context.Background(), state)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(engine.ensured) != 1 || engine.ensured[0] != "alloc-1" {
		t.Fatalf("unexpected ensured services: %#v", engine.ensured)
	}
	if _, err := os.Stat(filepath.Join(dir, "desired", "alloc-1.json")); err != nil {
		t.Fatalf("expected desired-state file: %v", err)
	}
	if report.Services[0].Phase != "Starting" {
		t.Fatalf("expected Starting phase, got %q", report.Services[0].Phase)
	}
}

func TestContainerdRuntimeReconcileRemovesStaleService(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	engine := &fakeEngine{status: map[string]serviceStatus{}, created: map[string]bool{}}
	runtime := &ContainerdRuntime{
		cfg: config.AgentConfig{
			Runtime: config.RuntimeConfig{
				DataDir:    dir,
				VolumesDir: filepath.Join(dir, "volumes"),
			},
		},
		engine: engine,
	}
	if err := os.MkdirAll(filepath.Join(dir, "desired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desired", "old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "volumes", "old-vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.Reconcile(context.Background(), &agentv1.DesiredNodeState{AgentId: "node-1"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(engine.removed) != 1 || engine.removed[0] != "old" {
		t.Fatalf("unexpected removed services: %#v", engine.removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "volumes", "old-vol")); !os.IsNotExist(err) {
		t.Fatalf("expected stale volume dir removal, stat err=%v", err)
	}
}
