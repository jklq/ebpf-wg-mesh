//go:build integration

package controlplane

import (
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func TestProjectSummariesSelectLiveLandingEnvironmentAndServices(t *testing.T) {
	store, ctx, userID, projectID, online := setupDirectImageServiceForDeployment(t)
	user := testUser(userID)
	production := productionEnvironmentID(t, store, projectID)
	staging, err := store.catalog.createEnvironment(ctx, user, projectID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	deletedEnv, err := store.catalog.createEnvironment(ctx, user, projectID, "removed")
	if err != nil {
		t.Fatal(err)
	}
	for _, environmentID := range []string{staging.ID, deletedEnv.ID} {
		if _, err := store.createStagedServiceForTest(ctx, userID, environmentID, "other-web", directImageServiceSpec("nginx:1.27", nil)); err != nil {
			t.Fatal(err)
		}
	}
	offline, err := store.createStagedServiceForTest(ctx, userID, production, "offline", directImageServiceSpec("nginx:1.27", nil))
	if err != nil {
		t.Fatal(err)
	}
	deletedService, err := store.createStagedServiceForTest(ctx, userID, production, "removed", directImageServiceSpec("nginx:1.27", nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `UPDATE environments SET deleted_at=$1, delete_expires_at=$2 WHERE id=$3`, now, now.Add(time.Hour), deletedEnv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE services SET deleted_at=$1, delete_expires_at=$2 WHERE id=$3`, now, now.Add(time.Hour), deletedService.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.catalog.createProject(ctx, testUser("stranger"), "", "private"); err != nil {
		t.Fatal(err)
	}

	var allocationReads int
	delivery := &fakePlatformDelivery{liveAllocationsFn: func(environmentID string) (map[string][]deliverycore.AllocationRecord, error) {
		allocationReads++
		if environmentID != production {
			t.Fatalf("health read for non-landing environment %q", environmentID)
		}
		return map[string][]deliverycore.AllocationRecord{
			online.ID: {{Healthy: true, AllocationIPv4: "10.0.0.1", AllocationIPv6: "fd00::1",
				DesiredSpecRevision: 1, AppliedSpecRevision: 1, DesiredRolloutGeneration: 1, AppliedRolloutGeneration: 1}},
			offline.ID: {{Healthy: true, AllocationIPv4: "10.0.0.2", AllocationIPv6: "fd00::2",
				DesiredSpecRevision: 1, AppliedSpecRevision: 1, DesiredRolloutGeneration: 2, AppliedRolloutGeneration: 1}},
		}, nil
	}}
	service := newPlatformService(store.platform(), noopNotifier{}, noopIngress{}, delivery)
	response, err := service.ListProjects(contextWithDelegatedUser(userID, ""), &platformv1.ListProjectsRequest{IncludeSummaries: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Projects) != 1 || len(response.Summaries) != 1 {
		t.Fatalf("overview includes foreign projects: %v", response)
	}
	summary := response.Summaries[0]
	if summary.Project.Id != projectID || summary.EnvironmentCount != 2 || summary.GetLandingEnvironment().GetId() != production {
		t.Fatalf("unexpected project summary: %v", summary)
	}
	if allocationReads != 1 || len(summary.Services) != 2 || summary.Services[0].Id != online.ID || !summary.Services[0].Online || summary.Services[1].Id != offline.ID || summary.Services[1].Online {
		t.Fatalf("unexpected landing services: %v, health reads=%d", summary.Services, allocationReads)
	}

	// When production is deleted, the first remaining environment becomes
	// the landing page. A caller without membership sees none of its details.
	if _, err := store.db.ExecContext(ctx, `UPDATE environments SET deleted_at=$1, delete_expires_at=$2 WHERE id=$3`, now, now.Add(time.Hour), production); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, user, "", false)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := store.catalog.listProjectSummaries(ctx, user, projects)
	if err != nil || len(summaries) != 1 || summaries[0].EnvironmentCount != 1 || summaries[0].GetLandingEnvironment().GetId() != staging.ID || len(summaries[0].Services) != 1 {
		t.Fatalf("fallback summaries: %v, %v", summaries, err)
	}
	summaries, err = store.catalog.listProjectSummaries(ctx, testUser("stranger"), projects)
	if err != nil || len(summaries) != 1 || summaries[0].EnvironmentCount != 0 || summaries[0].LandingEnvironment != nil || len(summaries[0].Services) != 0 {
		t.Fatalf("foreign membership exposed summary details: %v, %v", summaries, err)
	}
}
