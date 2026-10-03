//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"google.golang.org/protobuf/proto"
)

func TestServiceDraftReplacementRejectsStaleRevision(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapProjectAndAgent(t, store, ctx)
	d := testDelivery(store)
	service, err := d.CreateScheduledService(ctx, testUser("user-1"), productionEnvironmentID(t, store, projectID), "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{CpuMillis: 250}))
	if err != nil {
		t.Fatal(err)
	}
	cpuEdit := proto.Clone(service.Spec).(*platformv1.ServiceSpec)
	cpuEdit.Runtime.CpuMillis = 500
	updated, _, err := d.UpdateService(ctx, testUser("user-1"), service.ID, "", cpuEdit, service.SpecRevision)
	if err != nil {
		t.Fatal(err)
	}
	// The second editor read the original draft before the first edit committed.
	envEdit := proto.Clone(service.Spec).(*platformv1.ServiceSpec)
	envEdit.Runtime.Env = map[string]string{"MODE": "new"}
	if _, _, err := d.UpdateService(ctx, testUser("user-1"), service.ID, "", envEdit, service.SpecRevision); !errors.Is(err, deliverycore.ErrConcurrentUpdate) {
		t.Fatalf("stale draft replacement = %v", err)
	}
	current, err := d.ServiceByID(ctx, testUser("user-1"), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.SpecRevision != updated.SpecRevision || current.Spec.Runtime.CpuMillis != 500 || len(current.Spec.Runtime.Env) != 0 {
		t.Fatalf("stale edit changed the committed draft: revision=%d cpu=%d", current.SpecRevision, current.Spec.Runtime.CpuMillis)
	}
	// A fresh read lets the second editor apply its change without undoing CPU.
	current.Spec.Runtime.Env = map[string]string{"MODE": "new"}
	if _, _, err := d.UpdateService(ctx, testUser("user-1"), service.ID, "", current.Spec, current.SpecRevision); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentSnapshotIncludesServicesVolumesAndCommittedIndex(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	projectID := bootstrapProjectAndAgent(t, store, ctx)
	environmentID := productionEnvironmentID(t, store, projectID)
	d := testDelivery(store)
	service, err := d.CreateScheduledService(ctx, testUser("user-1"), environmentID, "web", directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{CpuMillis: 250}))
	if err != nil {
		t.Fatal(err)
	}
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), environmentID, "data", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.ReadEnvironmentSnapshot(ctx, testUser("user-1"), environmentID, false)
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.currentGlobalRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Index != index || len(snapshot.Services) != 1 || snapshot.Services[0].ID != service.ID || len(snapshot.Volumes) != 1 || snapshot.Volumes[0].ID != volume.ID || !snapshot.Volumes[0].Staged {
		t.Fatalf("environment snapshot = %+v", snapshot)
	}
	if _, err := d.ReadEnvironmentSnapshot(ctx, testUser("outsider"), environmentID, false); err == nil {
		t.Fatal("unauthorized environment snapshot succeeded")
	}

	// Commit edits after the read transaction establishes its snapshot. Neither
	// the draft details nor the volume may leak into the old revision's response.
	deps := deliveryDependencies(store, nil, nil, nil, nil)
	mutated := false
	deps.ReadState = func(ctx context.Context, read func(*sql.Tx, *journal.Projection) error) error {
		return crdb.ExecuteTx(ctx, store.db, nil, func(tx *sql.Tx) error {
			if _, err := dbtx.EnvironmentRevision(ctx, tx); err != nil {
				return err
			}
			if !mutated {
				edit := proto.Clone(service.Spec).(*platformv1.ServiceSpec)
				edit.Runtime.CpuMillis = 500
				if _, _, err := d.UpdateService(ctx, testUser("user-1"), service.ID, "", edit, service.SpecRevision); err != nil {
					return err
				}
				if _, err := d.GrowVolume(ctx, testUser("user-1"), volume.ID, 2<<30); err != nil {
					return err
				}
				mutated = true
			}
			return read(tx, nil)
		})
	}
	reader := deliverycore.New(deps)
	prior, err := reader.ReadEnvironmentSnapshot(ctx, testUser("user-1"), environmentID, false)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Index != index || prior.Services[0].SpecRevision != service.SpecRevision || prior.Services[0].Spec.Runtime.CpuMillis != 250 || prior.Volumes[0].SizeBytes != 1<<30 {
		t.Fatalf("read mixed committed snapshots: %+v", prior)
	}
	fresh, err := d.ReadEnvironmentSnapshot(ctx, testUser("user-1"), environmentID, false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Index <= prior.Index || fresh.Services[0].Spec.Runtime.CpuMillis != 500 || fresh.Volumes[0].SizeBytes != 2<<30 {
		t.Fatalf("next snapshot missed committed edits: %+v", fresh)
	}
}
