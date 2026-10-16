//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/billing"
	"ebof-wg-mesh/internal/controlplane/identity"

	"google.golang.org/protobuf/encoding/protojson"
)

// enabledBilling builds an enabled-mode billing service over the test store
// and turns on the persistence-layer gates the server wires from the mode.
func enabledBilling(t *testing.T, store *persistence) *billing.Service {
	t.Helper()
	store.enforceBilling = true
	svc := billing.New(store.db, store.source.Work(), store.withProductTx, nil, nil, billing.Config{Mode: billing.ModeEnabled})
	if _, err := svc.EnsureAccounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc
}

func creditBalance(t *testing.T, svc *billing.Service, workspaceID string) int64 {
	t.Helper()
	account, _, err := svc.AccountForWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	balance, err := svc.CreditBalance(context.Background(), account.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

func workspaceRestriction(t *testing.T, svc *billing.Service, workspaceID string) billing.Restriction {
	t.Helper()
	restriction, _, err := svc.RestrictionForWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return restriction
}

// (a) Trial credit is granted once per user: the first workspace receives
// it (with the default budget stop), later workspaces start restricted,
// and deleting the granted workspace does not restore eligibility.
func TestBillingTrialGrantedOncePerUser(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	user := testUser("user-2")

	// The user's first workspace is a shared one; no personal workspace
	// exists yet, so the shared workspace's creator owns the grant.
	first, err := store.catalog.createWorkspace(ctx, user, "First Shared")
	if err != nil {
		t.Fatal(err)
	}
	svc := enabledBilling(t, store)
	if balance := creditBalance(t, svc, first.ID); balance != billing.TrialCreditCents {
		t.Fatalf("first workspace credit = %d, want %d", balance, billing.TrialCreditCents)
	}
	account, _, err := svc.AccountForWorkspace(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !account.BudgetStopCents.Valid || account.BudgetStopCents.Int64 != billing.TrialCreditCents {
		t.Fatalf("first workspace budget stop = %+v, want %d", account.BudgetStopCents, billing.TrialCreditCents)
	}

	second, err := store.catalog.createWorkspace(ctx, user, "Second Shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsureAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	if balance := creditBalance(t, svc, second.ID); balance != 0 {
		t.Fatalf("second workspace credit = %d, want 0", balance)
	}
	if err := svc.EvaluateRestrictions(ctx); err != nil {
		t.Fatal(err)
	}
	if r := workspaceRestriction(t, svc, second.ID); !r.Enforced() || r.Reason != "trial credit already used" {
		t.Fatalf("second workspace restriction = %+v, want enforced trial-used", r)
	}
	if r := workspaceRestriction(t, svc, first.ID); r.Restricted {
		t.Fatalf("first workspace restricted = %+v, want unrestricted", r)
	}

	// Deleting the granted workspace must not hand the trial to the next one.
	if err := store.catalog.deleteWorkspace(ctx, user, first.ID, "First Shared"); err != nil {
		t.Fatal(err)
	}
	personal, err := store.catalog.ensurePersonalWorkspace(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsureAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	if balance := creditBalance(t, svc, personal.ID); balance != 0 {
		t.Fatalf("personal workspace credit after deletion = %d, want 0", balance)
	}
	if err := svc.EvaluateRestrictions(ctx); err != nil {
		t.Fatal(err)
	}
	if r := workspaceRestriction(t, svc, personal.ID); !r.Enforced() {
		t.Fatalf("personal workspace restriction = %+v, want enforced", r)
	}
	var grants int
	if err := store.db.QueryRow(`SELECT count(*) FROM billing_trial_grants WHERE user_id = 'user-2'`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("trial grants for user-2 = %d, want exactly 1", grants)
	}
}

// (c) The trial egress cap engages a normal restriction from hourly
// rollups in the maintenance evaluation, with its own reason.
func TestBillingTrialEgressCapRestricts(t *testing.T) {
	f := newBillingFixture(t, billing.ModeEnabled, nil)
	ctx := context.Background()
	hour := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO usage_attributions
		(resource_kind, resource_id, agent_id, workspace_id, project_id, environment_id, service_id, environment_name, resource_name, created_at)
		VALUES ('allocation', 'alloc-egress-1', 'node-a', $1, $2, 'env-1', 'svc-1', 'production', 'web', $3)`,
		f.workspace, f.project, f.anchor); err != nil {
		t.Fatal(err)
	}
	// 26 GiB of public egress rates to 130 cents, under the 500-cent
	// default budget stop, so only the egress cap can restrict.
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO usage_hourly
		(hour, resource_kind, resource_id, project_id, environment_id, service_id, records, measured_ms, gap_ms, uncertain_ms,
		 cpu_usec, memory_byte_ms, memory_covered_ms, volume_byte_ms, volume_covered_ms, retained_volume_byte_ms,
		 egress_public_direct, egress_public_ingress, egress_private, egress_platform, egress_probe, first_start, last_end)
		VALUES ($1, 'allocation', 'alloc-egress-1', $2, 'env-1', 'svc-1', 60, 3600000, 0, 0,
		 0, 0, 0, 0, 0, 0, $3, 0, 0, 0, 0, $1, $1)`,
		hour, f.project, int64(26)<<30); err != nil {
		t.Fatal(err)
	}
	f.setClock(time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	if err := f.svc.EvaluateRestrictions(ctx); err != nil {
		t.Fatal(err)
	}
	restriction := workspaceRestriction(t, f.svc, f.workspace)
	if !restriction.Enforced() || restriction.Reason != "trial egress limit reached" {
		t.Fatalf("restriction = %+v, want enforced trial-egress restriction", restriction)
	}
}

// (e) Trial compute requires a GitHub account at least 30 days old. The
// age evidence arrives on the user assertion and is recorded when the
// personal workspace is ensured; unknown ages fail closed, and users
// without a GitHub profile (bootstrap/dev) are never gated.
func TestBillingGitHubAgeGate(t *testing.T) {
	f := newBillingFixture(t, billing.ModeEnabled, nil)
	ctx := context.Background()

	evaluate := func() billing.Restriction {
		t.Helper()
		if err := f.svc.EvaluateRestrictions(ctx); err != nil {
			t.Fatal(err)
		}
		return workspaceRestriction(t, f.svc, f.workspace)
	}
	if r := evaluate(); r.Restricted {
		t.Fatalf("restriction without a GitHub profile = %+v, want unrestricted", r)
	}

	// Sign-in with a 10-day-old GitHub account records the profile and
	// the next evaluation restricts the trial workspace.
	young := f.anchor.Add(-10 * 24 * time.Hour)
	ghCtx := identity.WithDelegatedGitHubUser(ctx, "user-1", "user@example.com", young.Unix())
	if _, _, err := f.store.catalog.listWorkspaces(ghCtx, testUser("user-1"), "user@example.com"); err != nil {
		t.Fatal(err)
	}
	if r := evaluate(); !r.Enforced() || r.Reason != "github account is less than 30 days old" {
		t.Fatalf("young-account restriction = %+v, want enforced github-age restriction", r)
	}

	if _, err := f.store.db.Exec(`UPDATE user_profiles SET github_created_at = $2 WHERE user_id = $1`,
		"user-1", f.anchor.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := evaluate(); r.Restricted {
		t.Fatalf("old-account restriction = %+v, want unrestricted", r)
	}

	if _, err := f.store.db.Exec(`UPDATE user_profiles SET github_created_at = NULL WHERE user_id = $1`, "user-1"); err != nil {
		t.Fatal(err)
	}
	if r := evaluate(); !r.Enforced() {
		t.Fatalf("unknown-age restriction = %+v, want enforced (fail closed)", r)
	}
}

// (e) Custom domain bindings on the trial plan require a saved payment
// method; paid plans are unaffected.
func TestBillingCustomDomainRequiresPaymentMethod(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	_ = enabledBilling(t, store)
	service, err := createService(ctx, store, "user-1", envID, "web",
		directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{CpuMillis: 100, MemoryMebibytes: 128}), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	var workspace string
	if err := store.db.QueryRow(`SELECT workspace_id FROM projects WHERE id = $1`, service.ProjectID).Scan(&workspace); err != nil {
		t.Fatal(err)
	}

	// Platform-generated hostnames are not gated and are a precondition
	// for custom bindings.
	if _, _, err := store.routing.CreatePlatformDomainBindingRecord(ctx, testUser("user-1"), "web.apps.example.test", service.ID, 8080); err != nil {
		t.Fatalf("platform-generated domain on trial: %v", err)
	}
	_, _, err = store.routing.CreateDomainBindingRecord(ctx, testUser("user-1"), "app.example.com", service.ID, 8080)
	if !errors.Is(err, billing.ErrGrowthBlocked) {
		t.Fatalf("trial domain without payment method = %v, want ErrGrowthBlocked", err)
	}
	if _, err := store.db.Exec(`UPDATE billing_accounts SET payment_ready = TRUE WHERE workspace_id = $1`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("user-1"), "app.example.com", service.ID, 8080); err != nil {
		t.Fatalf("trial domain with payment method: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE billing_accounts SET plan_id = 'hobby', payment_ready = FALSE WHERE workspace_id = $1`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.routing.CreateDomainBindingRecord(ctx, testUser("user-1"), "www.example.com", service.ID, 8080); err != nil {
		t.Fatalf("paid-plan domain without payment method flag: %v", err)
	}
}

func quotaSpec(cpuMillis, memoryMebibytes int64, replicas int32) *platformv1.ServiceSpec {
	spec := directImageServiceSpec("example.test/web:1", &platformv1.ServiceRuntime{
		CpuMillis: cpuMillis, MemoryMebibytes: memoryMebibytes,
	})
	if replicas != 0 {
		spec.DesiredReplicaCount = &replicas
	}
	return spec
}

// (b) Every trial quota rejects the mutation that crosses it, and
// reductions stay allowed while the workspace is already over quota.
// Trial: 2000m CPU, 4096 MiB memory, 3 services per environment,
// 2 replicas per service, 2 projects, 10 GiB volumes.
func TestBillingTrialQuotasEnforcedOnMutations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	envID := seedReplicaFixture(t, store, ctx, []string{"node-a"})
	_ = enabledBilling(t, store)
	var workspace string
	if err := store.db.QueryRow(`SELECT p.workspace_id FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.id = $1`, envID).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	wantQuota := func(op string, err error) {
		t.Helper()
		if !errors.Is(err, billing.ErrGrowthBlocked) {
			t.Fatalf("%s = %v, want a quota (growth-blocked) error", op, err)
		}
	}
	mustCreate := func(name string, spec *platformv1.ServiceSpec) string {
		t.Helper()
		rec, err := createScheduledService(ctx, store, "user-1", envID, name, spec)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return rec.ID
	}

	s1 := mustCreate("s1", quotaSpec(1000, 1024, 1)) // totals 1000m / 1024 MiB
	_, err := createScheduledService(ctx, store, "user-1", envID, "s2", quotaSpec(1001, 128, 1))
	wantQuota("create over CPU quota", err)
	mustCreate("s2", quotaSpec(500, 3000, 1)) // totals 1500m / 4024 MiB
	_, err = createScheduledService(ctx, store, "user-1", envID, "s3", quotaSpec(100, 100, 1))
	wantQuota("create over memory quota", err)
	s3 := mustCreate("s3", quotaSpec(100, 10, 1)) // totals 1600m / 4034 MiB, 3 services
	_, err = createScheduledService(ctx, store, "user-1", envID, "s4", quotaSpec(10, 10, 1))
	wantQuota("create over environment service quota", err)

	if _, _, err := scaleService(ctx, store, "user-1", s3, 2); err != nil {
		t.Fatalf("scale s3 to 2: %v", err)
	}
	_, _, err = scaleService(ctx, store, "user-1", s3, 3)
	wantQuota("scale over replica quota", err)
	_, _, err = updateService(ctx, store, "user-1", s1, "s1", quotaSpec(1301, 1024, 1))
	wantQuota("update over CPU quota", err)

	// Projects: the seeded workspace holds one already.
	if _, err := store.catalog.createProject(ctx, testUser("user-1"), workspace, "p2"); err != nil {
		t.Fatalf("create project p2: %v", err)
	}
	if _, err := store.catalog.createProject(ctx, testUser("user-1"), workspace, "p2"); err != nil {
		t.Fatalf("re-ensure project p2: %v", err)
	}
	_, err = store.catalog.createProject(ctx, testUser("user-1"), workspace, "p3")
	wantQuota("create over project quota", err)

	// Volumes: 6 GiB fits, 5 more crosses 10 GiB, growing to exactly 10 fits.
	volume, err := store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "data-a", 6<<30)
	if err != nil {
		t.Fatalf("create volume data-a: %v", err)
	}
	_, err = store.catalog.createScheduledVolume(ctx, testUser("user-1"), envID, "data-b", 5<<30)
	wantQuota("create over volume quota", err)
	_, err = testDelivery(store).GrowVolume(ctx, testUser("user-1"), volume.ID, 11<<30)
	wantQuota("grow over volume quota", err)
	if _, err := testDelivery(store).GrowVolume(ctx, testUser("user-1"), volume.ID, 10<<30); err != nil {
		t.Fatalf("grow volume to quota limit: %v", err)
	}

	// Push the workspace over the CPU quota directly in desired state
	// (as a quota shrink or pre-quota state would), then prove reductions
	// still pass while any increase is refused.
	raw, err := protojson.Marshal(quotaSpec(5000, 1024, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE service_revisions SET spec_json = $1
		WHERE service_id = $2 AND spec_revision = (SELECT current_spec_revision FROM services WHERE id = $2)`, raw, s1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := updateService(ctx, store, "user-1", s1, "s1", quotaSpec(4000, 1024, 1)); err != nil {
		t.Fatalf("reduce CPU while over quota: %v", err)
	}
	if _, _, err := scaleService(ctx, store, "user-1", s3, 1); err != nil {
		t.Fatalf("scale down while over quota: %v", err)
	}
	_, err = createScheduledService(ctx, store, "user-1", envID, "s5", quotaSpec(10, 10, 1))
	wantQuota("create while over quota", err)
}

// (d) Trial build caps refuse claims at one concurrent build per
// workspace (across projects) and at 300 build-minutes per rolling
// 30 days; builds outside the window stop counting.
func TestBillingTrialBuildCapsRefuseClaims(t *testing.T) {
	store, ctx, _, _, service := setupSourceServiceForDeployment(t)
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), "", false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	second, err := store.catalog.createProject(ctx, testUser("user-1"), projects[0].WorkspaceID, "second")
	if err != nil {
		t.Fatal(err)
	}
	other, err := createService(ctx, store, "user-1", productionEnvironmentID(t, store, second.ID), "web-b", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-cap-1"); err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, other, "commit-cap-2"); err != nil {
		t.Fatal(err)
	}
	buildA, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-cap-1")
	if err != nil {
		t.Fatal(err)
	}
	buildB, err := enqueueBuildForTest(ctx, store, "user-1", other.ID, "commit-cap-2")
	if err != nil {
		t.Fatal(err)
	}

	_ = enabledBilling(t, store)
	d := testDelivery(store)
	claimed := claimBuildWith(t, d, ctx, "builder-1", buildA.ID)
	// The second project's build cannot claim: the workspace already has
	// its one trial build running.
	assertNoClaim(t, d, ctx, "builder-2")

	// Record 300 build-minutes inside the window on the finished build.
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs
		SET state = 'succeeded',
		    started_at = statement_timestamp() - interval '301 minutes',
		    finished_at = statement_timestamp() - interval '1 minute'
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	assertNoClaim(t, d, ctx, "builder-2")

	// Outside the rolling 30 days the minutes stop counting.
	if _, err := store.db.ExecContext(ctx, `UPDATE build_runs
		SET started_at = started_at - interval '31 days',
		    finished_at = finished_at - interval '31 days'
		WHERE id = $1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	claimBuildWith(t, d, ctx, "builder-2", buildB.ID)
}
