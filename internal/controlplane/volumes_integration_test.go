//go:build integration

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func volumeServiceSpec(volumeName, mountPath string) *platformv1.ServiceSpec {
	return directImageServiceSpec("example.test/db:1", &platformv1.ServiceRuntime{
		CpuMillis: 10, MemoryMebibytes: 16,
		Volume: &platformv1.ServiceVolumeMount{VolumeName: volumeName, MountPath: mountPath},
	})
}

func volumePinnedAgent(t *testing.T, store *persistence, volumeID string) string {
	t.Helper()
	var agentID string
	if err := store.db.QueryRowContext(context.Background(), `SELECT COALESCE(agent_id, '') FROM volumes WHERE id = $1`, volumeID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	return agentID
}

func desiredVolume(t *testing.T, store *persistence, agentID, volumeID string) *agentv1.DesiredVolume {
	t.Helper()
	state, err := desiredStateForAgent(context.Background(), store, agentID)
	if err != nil {
		t.Fatalf("desiredStateForAgent(%s): %v", agentID, err)
	}
	for _, volume := range state.GetVolumes() {
		if volume.GetVolumeId() == volumeID {
			return volume
		}
	}
	return nil
}

func listedVolume(t *testing.T, store *persistence, environmentID, volumeID string) *platformv1.Volume {
	t.Helper()
	volumes, err := store.reads.ListVolumes(context.Background(), testUser("user-1"), environmentID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range volumes {
		if volume.ID == volumeID {
			return toProtoVolume(testDelivery(store).RenderVolumeStatus(volume))
		}
	}
	t.Fatalf("volume %s not listed", volumeID)
	return nil
}

func reportVolume(t *testing.T, store *persistence, agentID string, conditions ...*agentv1.VolumeCondition) {
	t.Helper()
	report := &agentv1.StatusReport{AgentId: agentID, SessionId: "test-session-" + agentID, Volumes: conditions}
	if session, ok := fixtureLive(store).Session(agentID); ok {
		report.SessionId = session.SessionID
		report.ObservationSequence = session.Sequence + 1
	}
	if _, _, err := testDelivery(store).recordStatusReport(context.Background(), agentID, report); err != nil {
		t.Fatalf("recordStatusReport: %v", err)
	}
}

// A volume is pinned to the node of its first placement. Later services that
// mount it land there regardless of preference, and the volume stays desired
// on that node when no service runs.
func TestVolumePinsServicesToItsNode(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "pg-data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if pinned := volumePinnedAgent(t, store, volume.ID); pinned != "" {
		t.Fatalf("new volume pinned to %q before any placement", pinned)
	}
	if got := listedVolume(t, store, envID, volume.ID); got.GetState() != platformv1.VolumeState_VOLUME_STATE_PENDING {
		t.Fatalf("unplaced volume state = %s, want PENDING", got.GetState())
	}

	first, err := createService(ctx, store, "user-1", envID, "db", volumeServiceSpec("pg-data", "/var/lib/postgresql/data"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	allocations := mustListAllocations(t, store, ctx, first.ID)
	if len(allocations) != 1 {
		t.Fatalf("allocations = %d, want 1", len(allocations))
	}
	pinned := volumePinnedAgent(t, store, volume.ID)
	if pinned != allocations[0].AgentID {
		t.Fatalf("volume pinned to %q, allocation on %q", pinned, allocations[0].AgentID)
	}
	other := "node-b"
	if pinned == "node-b" {
		other = "node-a"
	}
	if desiredVolume(t, store, pinned, volume.ID) == nil {
		t.Fatal("pinned node does not desire its volume")
	}
	if desiredVolume(t, store, other, volume.ID) != nil {
		t.Fatal("volume leaked into another node's desired state")
	}
	state, err := desiredStateForAgent(ctx, store, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.GetServices()) != 1 || state.GetServices()[0].GetVolumeId() != volume.ID ||
		state.GetServices()[0].GetSpec().GetRuntime().GetVolume().GetMountPath() != "/var/lib/postgresql/data" {
		t.Fatalf("service mount not rendered: %+v", state.GetServices())
	}

	// No service running: the volume stays desired, not destroyed.
	if err := deleteService(ctx, store, "user-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if volume := desiredVolume(t, store, pinned, volume.ID); volume == nil || volume.GetDestroy() {
		t.Fatalf("volume without a service = %+v, want retained", volume)
	}

	// A new service prefers the other node but must follow the data.
	second, err := createService(ctx, store, "user-1", envID, "db-2", volumeServiceSpec("pg-data", "/data"), other)
	if err != nil {
		t.Fatal(err)
	}
	allocations = mustListAllocations(t, store, ctx, second.ID)
	if len(allocations) != 1 || allocations[0].AgentID != pinned {
		t.Fatalf("second service placed on %+v, want pinned node %s", allocations, pinned)
	}
}

func TestVolumeMountsAreExclusiveAndValidated(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	if _, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "shared", 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := createScheduledService(ctx, store, "user-1", envID, "bad-path", volumeServiceSpec("shared", "/etc/app")); !errors.Is(err, deliverycore.ErrInvalidVolumeMount) {
		t.Fatalf("system mount path: %v", err)
	}
	owner, err := createScheduledService(ctx, store, "user-1", envID, "owner", volumeServiceSpec("shared", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := owner.Spec.GetRuntime().GetVolume().GetMountPath(); got != "/data" {
		t.Fatalf("default mount path = %q, want /data", got)
	}
	if _, err := createScheduledService(ctx, store, "user-1", envID, "intruder", volumeServiceSpec("shared", "/data")); !errors.Is(err, deliverycore.ErrVolumeAttached) {
		t.Fatalf("second mount of an attached volume: %v", err)
	}

	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatal(err)
	}
	// Detaching in the draft does not free the volume while the deployed
	// revision still mounts it.
	detached := directImageServiceSpec("example.test/db:1", &platformv1.ServiceRuntime{CpuMillis: 10, MemoryMebibytes: 16})
	if _, _, err := updateService(ctx, store, "user-1", owner.ID, owner.Name, detached); err != nil {
		t.Fatal(err)
	}
	other, err := createScheduledService(ctx, store, "user-1", envID, "other", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := updateService(ctx, store, "user-1", other.ID, other.Name, volumeServiceSpec("shared", "/data")); !errors.Is(err, deliverycore.ErrVolumeAttached) {
		t.Fatalf("attach while the deployed revision mounts it: %v", err)
	}

	// The detach is a staged change.
	current, err := store.reads.ServiceByID(ctx, testUser("user-1"), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range current.UnappliedChanges {
		if change.GetId() == "runtime.volume" && change.GetCurrentValue() == "shared at /data" && change.GetNewValue() == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("detach is not a staged change: %+v", current.UnappliedChanges)
	}
}

func TestVolumeGrowsAndNeverShrinks(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "data", 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "user-1", envID, "db", volumeServiceSpec("data", "/data"), "node-a"); err != nil {
		t.Fatal(err)
	}
	delivery := testDelivery(store)
	if _, err := delivery.GrowVolume(ctx, testUser("user-1"), volume.ID, 64<<20); !errors.Is(err, deliverycore.ErrVolumeShrink) {
		t.Fatalf("shrink: %v", err)
	}
	if _, err := delivery.GrowVolume(ctx, testUser("user-1"), volume.ID, 256<<20+1); !errors.Is(err, deliverycore.ErrInvalidVolumeSize) {
		t.Fatalf("unaligned size: %v", err)
	}
	grown, err := delivery.GrowVolume(ctx, testUser("user-1"), volume.ID, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if grown.SizeBytes != 256<<20 {
		t.Fatalf("grown size = %d", grown.SizeBytes)
	}
	if desired := desiredVolume(t, store, "node-a", volume.ID); desired.GetSizeBytes() != 256<<20 {
		t.Fatalf("pinned node desires %d bytes, want 256MiB", desired.GetSizeBytes())
	}
}

// Data is deleted only by an explicit destroy instruction after the deletion
// grace, and the instruction retires once the node reports the data gone.
func TestVolumeDeletionDestroysDataOnlyAfterGrace(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	project, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("user-1"), project[0].ID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	_ = envID
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), staging.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", staging.ID, "db", volumeServiceSpec("data", "/data"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, testUser("user-1"), volume.ID, "data"); err != nil {
		t.Fatal(err)
	}
	// During the grace the node keeps the data and receives no instruction.
	if desired := desiredVolume(t, store, "node-a", volume.ID); desired != nil {
		t.Fatalf("tombstoned volume during grace = %+v, want absent", desired)
	}

	expireTombstone(t, store, "volumes", "id", volume.ID)
	if _, err := newDeletionGC(store, nil, nil, time.Minute).CollectOnce(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	desired := desiredVolume(t, store, "node-a", volume.ID)
	if desired == nil || !desired.GetDestroy() {
		t.Fatalf("expired volume = %+v, want an explicit destroy", desired)
	}

	reportVolume(t, store, "node-a", &agentv1.VolumeCondition{VolumeId: volume.ID, Phase: "Destroyed"})
	if desired := desiredVolume(t, store, "node-a", volume.ID); desired != nil {
		t.Fatalf("completed destruction still desired: %+v", desired)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM volume_destructions WHERE volume_id = $1`, volume.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("destruction row survived the node's report")
	}
}

func TestEnvironmentCollectionDestroysPinnedVolumes(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	seedReplicaFixture(t, store, ctx, []string{"node-a"})
	project, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.catalog.createEnvironment(ctx, testUser("user-1"), project[0].ID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), staging.ID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createService(ctx, store, "user-1", staging.ID, "db", volumeServiceSpec("data", "/data"), "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.deleteEnvironment(ctx, testUser("user-1"), staging.ID, "staging"); err != nil {
		t.Fatal(err)
	}
	expireTombstone(t, store, "environments", "id", staging.ID)
	if _, err := newDeletionGC(store, nil, nil, time.Minute).CollectOnce(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if desired := desiredVolume(t, store, "node-a", volume.ID); desired == nil || !desired.GetDestroy() {
		t.Fatalf("volume of a collected environment = %+v, want an explicit destroy", desired)
	}
}

func TestVolumeStatusReflectsNodeReportsAndPresence(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a", "node-b"})
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "data", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	service, err := createService(ctx, store, "user-1", envID, "db", volumeServiceSpec("data", "/data"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	pinned := volumePinnedAgent(t, store, volume.ID)
	if got := listedVolume(t, store, envID, volume.ID); got.GetState() != platformv1.VolumeState_VOLUME_STATE_PENDING || got.GetAgentId() != pinned {
		t.Fatalf("unreported volume = %s on %q, want PENDING on %s", got.GetState(), got.GetAgentId(), pinned)
	}

	reportVolume(t, store, pinned, &agentv1.VolumeCondition{VolumeId: volume.ID, Phase: "Ready", UsedBytes: 5 << 20, CapacityBytes: 60 << 20})
	got := listedVolume(t, store, envID, volume.ID)
	if got.GetState() != platformv1.VolumeState_VOLUME_STATE_READY || got.GetUsedBytes() != 5<<20 {
		t.Fatalf("reported volume = %s used %d, want READY used 5MiB", got.GetState(), got.GetUsedBytes())
	}

	reportVolume(t, store, pinned, &agentv1.VolumeCondition{VolumeId: volume.ID, Phase: "Full", UsedBytes: 60 << 20, CapacityBytes: 60 << 20},
		&agentv1.VolumeCondition{VolumeId: "left-behind", Phase: "Orphaned", UsedBytes: 1 << 20})
	if got := listedVolume(t, store, envID, volume.ID); got.GetState() != platformv1.VolumeState_VOLUME_STATE_FULL {
		t.Fatalf("full volume state = %s", got.GetState())
	}
	if orphans := testDelivery(store).OrphanedVolumes(pinned); len(orphans) != 1 || orphans[0] != "left-behind" {
		t.Fatalf("orphans = %v, want [left-behind]", orphans)
	}

	fixtureLive(store).SetLastContactForTest(pinned, time.Now().UTC().Add(-2*deliverycore.AgentHealthyTTL))
	got = listedVolume(t, store, envID, volume.ID)
	if got.GetState() != platformv1.VolumeState_VOLUME_STATE_UNAVAILABLE || !strings.Contains(got.GetStateMessage(), "unreachable") {
		t.Fatalf("lost node volume = %s %q, want UNAVAILABLE", got.GetState(), got.GetStateMessage())
	}

	// Re-placing the service must not start it elsewhere with an empty volume.
	if err := deleteService(ctx, store, "user-1", service.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := createService(ctx, store, "user-1", envID, "db-2", volumeServiceSpec("data", "/data"), "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if allocations := mustListAllocations(t, store, ctx, replacement.ID); len(allocations) != 0 {
		t.Fatalf("service started away from its volume: %+v", allocations)
	}
	current, err := store.reads.ServiceByID(ctx, testUser("user-1"), replacement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current.PlacementMessage, `volume "data" is on node`) {
		t.Fatalf("placement message = %q, want the volume's node", current.PlacementMessage)
	}
}

// Creating a volume is a staged change: releasing commits it, and discarding
// the draft that mounts it removes it, since nothing exists on any node yet.
func TestStagedVolumeFollowsItsDraft(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	delivery := testDelivery(store)
	user := testUser("user-1")
	volumeExists := func(id string) bool {
		var exists bool
		if err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}

	// Discarding a new service discards the new volume it mounts.
	discarded, err := store.catalog.createScheduledVolume(ctx, user, envID, "scratch", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !listedVolume(t, store, envID, discarded.ID).GetStaged() {
		t.Fatal("new volume is not staged")
	}
	draft, err := createScheduledService(ctx, store, "user-1", envID, "draft", volumeServiceSpec("scratch", "/data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.DeleteService(ctx, user, draft.ID); err != nil {
		t.Fatal(err)
	}
	if volumeExists(discarded.ID) {
		t.Fatal("staged volume survived discarding the service that mounted it")
	}

	// Releasing commits it; discards no longer touch it.
	kept, err := store.catalog.createScheduledVolume(ctx, user, envID, "kept", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := createScheduledService(ctx, store, "user-1", envID, "owner", volumeServiceSpec("kept", "/data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatal(err)
	}
	if listedVolume(t, store, envID, kept.ID).GetStaged() {
		t.Fatal("release did not commit the staged volume")
	}

	// Discarding a staged attach on a deployed service discards the volume.
	attached, err := store.catalog.createScheduledVolume(ctx, user, envID, "extra", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	web, err := createScheduledService(ctx, store, "user-1", envID, "web", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseEnvironmentForTest(ctx, store, "user-1", envID); err != nil {
		t.Fatal(err)
	}
	if !volumeExists(attached.ID) {
		t.Fatal("unmounted staged volume was dropped by an unrelated release")
	}
	if listedVolume(t, store, envID, attached.ID).GetStaged() {
		t.Fatal("release did not commit an unmounted staged volume")
	}
	late, err := store.catalog.createScheduledVolume(ctx, user, envID, "late", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := updateService(ctx, store, "user-1", web.ID, web.Name, volumeServiceSpec("late", "/data")); err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.DiscardServiceChanges(ctx, user, web.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if volumeExists(late.ID) {
		t.Fatal("staged volume survived discarding the attach")
	}
	if _, err := delivery.DiscardServiceChanges(ctx, user, owner.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if !volumeExists(kept.ID) {
		t.Fatal("discard removed a released volume")
	}

	// A staged volume nothing mounts is discarded without confirmation.
	loose, err := store.catalog.createScheduledVolume(ctx, user, envID, "loose", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.catalog.deleteVolume(ctx, user, loose.ID, ""); err != nil {
		t.Fatal(err)
	}
	if volumeExists(loose.ID) {
		t.Fatal("discarded staged volume left a row behind")
	}
}
