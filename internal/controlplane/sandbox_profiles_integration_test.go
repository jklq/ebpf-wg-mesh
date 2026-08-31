//go:build integration

package controlplane

import (
	"context"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestSandboxCompatibilityProfileAuditSurvivesServiceDeletion(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{Users: []config.BootstrapUser{{
		ID: "user-1", Email: "user@example.com", Projects: []string{"demo"},
	}}}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)
	relaxed := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		SandboxProfile: &platformv1.SandboxProfile{
			Name: "legacy-root",
			Risk: "The image runs as root.",
			Relaxations: []platformv1.SandboxRelaxation{
				platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT,
			},
		},
	})
	service, err := store.createScheduledService(ctx, "user-1", environmentID, "web", relaxed)
	if err != nil {
		t.Fatalf("createScheduledService: %v", err)
	}

	production := directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		SandboxProfile: productionSandboxProfile(),
	})
	if _, changed, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, "", production); err != nil || !changed {
		t.Fatalf("updateService: changed=%v err=%v", changed, err)
	}

	type auditEvent struct {
		actor, action, previous, profile, risk string
	}
	rows, err := store.db.QueryContext(ctx, `
		SELECT actor_user_id, action, previous_profile_name, profile_name, risk
		  FROM sandbox_profile_audit_events
		 WHERE service_id = $1
		 ORDER BY created_at, id`, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []auditEvent
	for rows.Next() {
		var event auditEvent
		if err := rows.Scan(&event.actor, &event.action, &event.previous, &event.profile, &event.risk); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("audit events = %#v", events)
	}
	if events[0] != (auditEvent{actor: "user-1", action: "selected", profile: "legacy-root", risk: "The image runs as root."}) {
		t.Fatalf("selected event = %#v", events[0])
	}
	if events[1] != (auditEvent{actor: "user-1", action: "changed", previous: "legacy-root", profile: "production"}) {
		t.Fatalf("changed event = %#v", events[1])
	}

	if err := store.deleteService(ctx, "user-1", projects[0].ID, service.ID); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_profile_audit_events WHERE service_id = $1`, service.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		t.Fatalf("retained audit events = %d, want 2", retained)
	}
}
