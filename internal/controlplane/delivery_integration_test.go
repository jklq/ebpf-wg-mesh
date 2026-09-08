//go:build integration

package controlplane

import (
	"context"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDeliveryReleaseAuthorizationAndAtomicity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, "owner", "delivery")
	if err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, project.ID)
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	first, err := createScheduledService(ctx, store, "owner", environmentID, "first", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := createScheduledService(ctx, store, "owner", environmentID, "second", directImageServiceSpec("example.test/web:1", nil))
	if err != nil {
		t.Fatal(err)
	}
	// Release visits IDs in order. Make the last draft invalid so the first
	// service's rollout and allocation must roll back when validation fails.
	if first.ID > second.ID {
		first, second = second, first
	}
	volume, err := store.catalog.createVolume(ctx, "owner", environmentID, "missing", 64<<20, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := updateService(ctx, store, "owner", second.ID, second.Name, directImageServiceSpec("example.test/web:2", &platformv1.ServiceRuntime{VolumeName: "missing"})); err != nil {
		t.Fatal(err)
	}
	// Simulate a resource disappearing after the draft was validated.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM volumes WHERE id = $1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO project_memberships(user_id, project_id, role) VALUES ('viewer', $1, 'viewer')`, project.ID); err != nil {
		t.Fatal(err)
	}
	notifier := &releaseTestNotifier{}
	delivery := newTestDelivery(store, notifier, nil, nil)
	before := mustDesiredRevision(t, store, ctx, "node-1")
	events := NewPlatformEvents(store.events, 0)
	eventBefore, err := events.Current(ctx, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	deploymentCounts := make(map[string]int)
	for _, id := range []string{first.ID, second.ID} {
		deployments, err := store.reads.listServiceDeployments(ctx, "owner", id, 10)
		if err != nil {
			t.Fatal(err)
		}
		deploymentCounts[id] = len(deployments)
	}
	if _, err := delivery.ReleaseEnvironment(contextWithDelegatedUser("owner", ""), environmentID); err == nil {
		t.Fatal("release with missing volume unexpectedly succeeded")
	}
	for _, id := range []string{first.ID, second.ID} {
		service, err := store.reads.serviceByID(ctx, "owner", id)
		if err != nil {
			t.Fatal(err)
		}
		if service.RolloutGeneration != 0 || service.AllocatedAgentID != "" {
			t.Fatalf("failed release mutated service: %#v", service)
		}
		deployments, err := store.reads.listServiceDeployments(ctx, "owner", id, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(deployments) != deploymentCounts[id] {
			t.Fatalf("failed release left deployments: %#v", deployments)
		}
	}
	if len(notifier.agentIDs) != 0 {
		t.Fatalf("failed release notified agents: %v", notifier.agentIDs)
	}
	if got := mustDesiredRevision(t, store, ctx, "node-1"); got != before {
		t.Fatalf("failed release changed desired revision: %d -> %d", before, got)
	}
	if got, err := events.Current(ctx, environmentID); err != nil || got != eventBefore {
		t.Fatalf("failed release changed event index: %d -> %d (%v)", eventBefore, got, err)
	}
	if _, _, err := updateService(ctx, store, "owner", second.ID, second.Name, directImageServiceSpec("example.test/web:2", nil)); err != nil {
		t.Fatal(err)
	}
	// Check authorization with valid drafts, so validation cannot mask a bypass.
	for _, user := range []string{"", "viewer", "outsider"} {
		releaseCtx := ctx
		if user != "" {
			releaseCtx = contextWithDelegatedUser(user, "")
		}
		if _, err := delivery.ReleaseEnvironment(releaseCtx, environmentID); err == nil {
			t.Fatalf("release by %q unexpectedly succeeded", user)
		} else if user == "" && status.Code(err) != codes.Unauthenticated {
			t.Fatalf("unauthenticated release: %v", err)
		}
	}
	released, err := delivery.ReleaseEnvironment(contextWithDelegatedUser("owner", ""), environmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 || len(notifier.agentIDs) != 1 {
		t.Fatalf("release result: %v, notifications: %v", released, notifier.agentIDs)
	}
	for _, result := range released {
		if result.Service.RolloutGeneration != 1 || len(result.Allocations) != 1 {
			t.Fatalf("incomplete release snapshot: %#v", result)
		}
	}
	notifier.agentIDs = nil
	released, err = delivery.ReleaseEnvironment(contextWithDelegatedUser("owner", ""), environmentID)
	if err != nil || len(released) != 0 || len(notifier.agentIDs) != 0 {
		t.Fatalf("unchanged release: %v, %v, %v", released, notifier.agentIDs, err)
	}
}
