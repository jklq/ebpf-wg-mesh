//go:build integration

package controlplane

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/source"
)

func testDigest(nibble string) string {
	return "sha256:" + strings.Repeat(nibble, 64)
}

func testPinnedRef(repository, nibble string) string {
	return repository + "@" + testDigest(nibble)
}

// TestBuildCompletionRecordsImmutableArtifact proves every successful build
// records the immutable artifact contract: source snapshot digest, commit
// SHA, build recipe and builder version, image manifest digest, build
// actor, and timestamps — and that the artifact, not an image string, is
// the deployment's runtime identity.
func TestBuildCompletionRecordsImmutableArtifact(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	image := testPinnedRef("registry.example.test/platform/web", "1")
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", image, ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}

	artifacts, err := testDelivery(store).ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil {
		t.Fatalf("ListServiceArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(artifacts))
	}
	artifact := artifacts[0]
	if artifact.Kind != deliverycore.BuildArtifactBuild {
		t.Fatalf("artifact kind = %q, want %q", artifact.Kind, deliverycore.BuildArtifactBuild)
	}
	if artifact.ServiceID != service.ID || artifact.BuildID != build.ID {
		t.Fatalf("artifact identity = (%s, %s), want (%s, %s)", artifact.ServiceID, artifact.BuildID, service.ID, build.ID)
	}
	if artifact.SourceSnapshotDigest == "" || artifact.SourceSnapshotDigest != build.SourceSnapshotDigest {
		t.Fatalf("artifact snapshot digest = %q, want build snapshot %q", artifact.SourceSnapshotDigest, build.SourceSnapshotDigest)
	}
	if artifact.CommitSHA != "commit-1" {
		t.Fatalf("artifact commit = %q", artifact.CommitSHA)
	}
	if artifact.BuildRecipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE ||
		artifact.BuildRecipe.GetDockerfilePath() != "Dockerfile" || artifact.BuildRecipe.GetContextDir() != "." {
		t.Fatalf("artifact build recipe = %+v", artifact.BuildRecipe)
	}
	if artifact.BuilderVersion != deliverycore.BuilderToolchainVersion || artifact.BuilderVersion == "" {
		t.Fatalf("artifact builder version = %q", artifact.BuilderVersion)
	}
	if artifact.ImageRepository != "registry.example.test/platform/web" ||
		artifact.ImageManifestDigest != testDigest("1") || artifact.ImageRef != image {
		t.Fatalf("artifact image identity = (%q, %q, %q)", artifact.ImageRepository, artifact.ImageManifestDigest, artifact.ImageRef)
	}
	if artifact.SourceImageRef != "" {
		t.Fatalf("build artifact must not record user image input, got %q", artifact.SourceImageRef)
	}
	if artifact.BuildActorKind != deliverycore.DeploymentCauseWebhook {
		t.Fatalf("artifact build actor kind = %q, want %q", artifact.BuildActorKind, deliverycore.DeploymentCauseWebhook)
	}
	if artifact.CreatedAt.IsZero() {
		t.Fatal("artifact created_at is unset")
	}

	completed, err := store.reads.BuildByID(ctx, build.ID)
	if err != nil {
		t.Fatalf("BuildByID: %v", err)
	}
	if completed.ArtifactID != artifact.ID || completed.ImageDigest != image {
		t.Fatalf("completed build = artifact %q image %q, want %q %q", completed.ArtifactID, completed.ImageDigest, artifact.ID, image)
	}
	dep := currentDeploymentForTest(t, store, ctx, service.ID)
	if dep.ArtifactID != artifact.ID || dep.ImageDigest != image || dep.Artifact == nil {
		t.Fatalf("deployment identity = artifact %q image %q, want %q %q", dep.ArtifactID, dep.ImageDigest, artifact.ID, image)
	}
	if _, err := testDelivery(store).ListServiceArtifacts(ctx, testUser("stranger"), service.ID, 10); err == nil {
		t.Fatal("ListServiceArtifacts allowed an unauthorized user")
	}
}

// TestDirectImageTagMutationAfterResolveKeepsStoredDigest is the required
// deploy-by-digest regression: a mutable tag resolves to a digest at deploy
// time, and moving the tag afterwards never changes what runs.
func TestDirectImageTagMutationAfterResolveKeepsStoredDigest(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	resolver := &registry.StaticResolver{Tags: map[string]string{
		"example.test/web:stable": testDigest("a"),
	}}
	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.SetImageResolver(resolver)

	const tagInput = "example.test/web:stable"
	service, err := delivery.CreateService(ctx, testUser("user-1"), productionEnvironmentID(t, store, projects[0].ID), "web",
		directImageServiceSpec(tagInput, &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}), "node-1")
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	pinnedA := testPinnedRef("example.test/web", "a")

	artifacts, err := delivery.ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts after create = %v, %v", artifacts, err)
	}
	if artifacts[0].Kind != deliverycore.BuildArtifactDirectImage || artifacts[0].ImageRef != pinnedA {
		t.Fatalf("direct-image artifact = %+v", artifacts[0])
	}
	if artifacts[0].SourceImageRef != tagInput || artifacts[0].BuildID != "" {
		t.Fatalf("direct-image artifact must record %q as user input only, got %+v", tagInput, artifacts[0])
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID).ImageDigest; got != pinnedA {
		t.Fatalf("deployment image = %q, want pinned %q", got, pinnedA)
	}
	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(state.GetServices()) != 1 {
		t.Fatalf("desiredStateForAgent: %v, %v", state, err)
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != pinnedA {
		t.Fatalf("desired image = %q, want pinned %q", got, pinnedA)
	}

	// The registry tag moves. Nothing on the platform re-resolves it: the
	// stored digest still runs.
	resolver.Tags[tagInput] = testDigest("b")
	state, err = desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent after tag mutation: %v", err)
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != pinnedA {
		t.Fatalf("tag mutation changed the running image: got %q, want stored digest %q", got, pinnedA)
	}

	// A fresh release resolves the tag anew and pins the new digest; the
	// superseded deployment keeps running its own stored digest.
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", directImageServiceSpec(tagInput, &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8080}), Env: map[string]string{"STAGE": "two"},
	})); err != nil {
		t.Fatalf("updateService: %v", err)
	}
	if _, err := delivery.ReleaseEnvironment(ctx, testUser("user-1"), service.EnvironmentID); err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	pinnedB := testPinnedRef("example.test/web", "b")
	artifacts, err = delivery.ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("artifacts after release = %v, %v", artifacts, err)
	}
	if artifacts[0].ImageRef != pinnedB || artifacts[0].SourceImageRef != tagInput {
		t.Fatalf("re-resolved artifact = %+v, want %q pinned to %q", artifacts[0], tagInput, pinnedB)
	}
	if artifacts[1].ImageRef != pinnedA {
		t.Fatalf("original artifact was rewritten: %+v", artifacts[1])
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID).ImageDigest; got != pinnedB {
		t.Fatalf("released deployment image = %q, want re-resolved %q", got, pinnedB)
	}
	state, err = desiredStateForAgent(ctx, store, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent after release: %v", err)
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != pinnedB {
		t.Fatalf("desired image after release = %q, want %q", got, pinnedB)
	}
}

// TestSameSourceReusesBuiltImageWithCurrentVariables proves the skip-rebuild
// contract: when the same source has already produced an image, no builder
// work is queued and the existing image deploys with the service's current
// variables instead of the original build's.
func TestSameSourceReusesBuiltImageWithCurrentVariables(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState: %v", err)
	}
	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build.ID)
	image := testPinnedRef("registry.example.test/platform/web", "5")
	if err := completeBuildForTest(ctx, store, "builder-1", build.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", image, ""); err != nil {
		t.Fatalf("completeBuild: %v", err)
	}
	artifacts, err := testDelivery(store).ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts after build = %v, %v", artifacts, err)
	}

	// The user changes variables; the source is unchanged.
	updated := repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080}), Env: map[string]string{"STAGE": "two"}},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	)
	if _, _, err := updateService(ctx, store, "user-1", service.ID, "", updated); err != nil {
		t.Fatalf("updateService: %v", err)
	}

	// The same commit is queued again (webhook redelivery). The build is
	// skipped and the existing image rolls out.
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}
	queued, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, source.BuildTransition{PreviousCommit: "commit-parent"})
	if err != nil {
		t.Fatalf("QueueSourceBuild: %v", err)
	}
	if !queued.Reused {
		t.Fatalf("expected the built image to be reused, got %+v", queued)
	}
	if queued.BuildID != build.ID || queued.DeploymentID == "" {
		t.Fatalf("reused queue result = %+v, want build %q", queued, build.ID)
	}

	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 1 {
		t.Fatalf("build_runs rows = %d, want 1 (skip rebuild)", buildCount)
	}
	dep := currentDeploymentForTest(t, store, ctx, service.ID)
	if dep.ID != queued.DeploymentID || dep.ArtifactID != artifacts[0].ID || dep.ImageDigest != image {
		t.Fatalf("reused deployment = %+v, want artifact %q image %q", dep, artifacts[0].ID, image)
	}
	if dep.ReasonCode != "BUILD_REUSED" {
		t.Fatalf("reused deployment reason = %q, want BUILD_REUSED", dep.ReasonCode)
	}
	if got := dep.ResolvedSpec.GetRuntime().GetEnv()["STAGE"]; got != "two" {
		t.Fatalf("reused deployment runs with env STAGE=%q, want the current variables STAGE=two", got)
	}
	state, err := desiredStateForAgent(ctx, store, "node-1")
	if err != nil || len(state.GetServices()) != 1 {
		t.Fatalf("desiredStateForAgent: %v, %v", state, err)
	}
	if got := state.GetServices()[0].GetSpec().GetImage(); got != image {
		t.Fatalf("desired image = %q, want reused %q", got, image)
	}
}

// TestReleaseEnvironmentSkipsUnchangedDirectImageTags proves one
// service's stale or unavailable tag cannot block releasing unrelated
// pending changes: only the services a release selects resolve tags.
func TestReleaseEnvironmentSkipsUnchangedDirectImageTags(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)

	resolver := &registry.StaticResolver{Tags: map[string]string{
		"example.test/a:1": testDigest("a"),
		"example.test/b:1": testDigest("b"),
	}}
	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.SetImageResolver(resolver)

	serviceA, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-a",
		directImageServiceSpec("example.test/a:1", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}), "node-1")
	if err != nil {
		t.Fatalf("CreateService svc-a: %v", err)
	}
	serviceB, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-b",
		directImageServiceSpec("example.test/b:1", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})}), "node-1")
	if err != nil {
		t.Fatalf("CreateService svc-b: %v", err)
	}

	// svc-a's image disappears from the registry while svc-b has pending
	// changes. The release must never resolve svc-a's unchanged tag.
	delete(resolver.Tags, "example.test/a:1")
	if _, _, err := updateService(ctx, store, "user-1", serviceB.ID, "", directImageServiceSpec("example.test/b:1", &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}), Env: map[string]string{"STAGE": "two"},
	})); err != nil {
		t.Fatalf("updateService svc-b: %v", err)
	}
	if _, err := delivery.ReleaseEnvironment(ctx, testUser("user-1"), environmentID); err != nil {
		t.Fatalf("ReleaseEnvironment: an unchanged service's unavailable tag blocked the release: %v", err)
	}

	pinnedA := testPinnedRef("example.test/a", "a")
	if got := currentDeploymentForTest(t, store, ctx, serviceA.ID).ImageDigest; got != pinnedA {
		t.Fatalf("svc-a image = %q, want untouched %q", got, pinnedA)
	}
	artifactsA, err := delivery.ListServiceArtifacts(ctx, testUser("user-1"), serviceA.ID, 10)
	if err != nil || len(artifactsA) != 1 || artifactsA[0].ImageRef != pinnedA {
		t.Fatalf("svc-a artifacts = %v, %v", artifactsA, err)
	}
	if got := currentDeploymentForTest(t, store, ctx, serviceB.ID).ImageDigest; got != testPinnedRef("example.test/b", "b") {
		t.Fatalf("svc-b image = %q, want %q", got, testPinnedRef("example.test/b", "b"))
	}
}

// TestReleaseEnvironmentResolvesOutsideSchedulerLock proves a slow or
// unreachable registry cannot stall scheduler-serialized mutations:
// tag resolution runs outside the scheduler lock and only the release
// transaction serializes with other scheduler work.
func TestReleaseEnvironmentResolvesOutsideSchedulerLock(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)

	const slowTag = "example.test/slow:1"
	resolver := &blockingTagResolver{
		StaticResolver: registry.StaticResolver{Tags: map[string]string{
			"example.test/a:1": testDigest("a"),
			"example.test/b:1": testDigest("b"),
			slowTag:            testDigest("c"),
		}},
		block:   slowTag,
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.SetImageResolver(resolver)

	serviceB, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-b",
		directImageServiceSpec("example.test/b:1", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})}), "node-1")
	if err != nil {
		t.Fatalf("CreateService svc-b: %v", err)
	}
	// The pending change deploys the blocking tag, so the release
	// resolves it and hangs in the "registry".
	if _, _, err := updateService(ctx, store, "user-1", serviceB.ID, "", directImageServiceSpec(slowTag, &platformv1.ServiceRuntime{
		Ports: runtimePortsFromInts([]int32{8081}), Env: map[string]string{"STAGE": "two"},
	})); err != nil {
		t.Fatalf("updateService svc-b: %v", err)
	}

	releaseErr := make(chan error, 1)
	go func() {
		_, err := delivery.ReleaseEnvironment(ctx, testUser("user-1"), environmentID)
		releaseErr <- err
	}()
	select {
	case <-resolver.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("release never reached the registry")
	}

	// While the registry is slow, scheduler-serialized mutations still
	// run: creating another service must not wait for the resolver.
	created := make(chan error, 1)
	go func() {
		_, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-a",
			directImageServiceSpec("example.test/a:1", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}), "node-1")
		created <- err
	}()
	select {
	case err := <-created:
		if err != nil {
			close(resolver.unblock)
			t.Fatalf("concurrent CreateService: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(resolver.unblock)
		t.Fatal("scheduler mutation stalled behind registry resolution")
	}
	close(resolver.unblock)

	if err := <-releaseErr; err != nil {
		t.Fatalf("ReleaseEnvironment: %v", err)
	}
	pinned := testPinnedRef("example.test/slow", "c")
	if got := currentDeploymentForTest(t, store, ctx, serviceB.ID).ImageDigest; got != pinned {
		t.Fatalf("svc-b image = %q, want pinned %q", got, pinned)
	}
}

// TestCreateServiceResolvesOutsideSchedulerLock proves service creation
// resolves direct-image tags outside the scheduler lock as well: a slow
// or unreachable registry must not stall unrelated scheduler work while
// a create waits on its pin.
func TestCreateServiceResolvesOutsideSchedulerLock(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	environmentID := productionEnvironmentID(t, store, projects[0].ID)

	const slowTag = "example.test/slow:1"
	resolver := &blockingTagResolver{
		StaticResolver: registry.StaticResolver{Tags: map[string]string{
			"example.test/a:1": testDigest("a"),
			slowTag:            testDigest("c"),
		}},
		block:   slowTag,
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	delivery := newTestDelivery(store, nil, nil, nil)
	delivery.SetImageResolver(resolver)

	created := make(chan error, 1)
	go func() {
		_, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-slow",
			directImageServiceSpec(slowTag, &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8081})}), "node-1")
		created <- err
	}()
	select {
	case <-resolver.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("create never reached the registry")
	}

	proceed := make(chan error, 1)
	go func() {
		_, err := delivery.CreateService(ctx, testUser("user-1"), environmentID, "svc-fast",
			directImageServiceSpec("example.test/a:1", &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}), "node-1")
		proceed <- err
	}()
	select {
	case err := <-proceed:
		if err != nil {
			close(resolver.unblock)
			t.Fatalf("concurrent CreateService: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(resolver.unblock)
		t.Fatal("scheduler mutation stalled behind registry resolution")
	}
	close(resolver.unblock)

	if err := <-created; err != nil {
		t.Fatalf("CreateService: %v", err)
	}
}

// blockingTagResolver hangs on one tag until released, simulating a slow
// or unreachable registry during resolution.
type blockingTagResolver struct {
	registry.StaticResolver
	block   string
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (r *blockingTagResolver) Resolve(ctx context.Context, ref string) (registry.ResolvedImage, error) {
	if strings.TrimSpace(ref) == r.block {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-r.unblock:
		case <-ctx.Done():
			return registry.ResolvedImage{}, ctx.Err()
		}
	}
	return r.StaticResolver.Resolve(ctx, ref)
}

// TestLateWebhookRevisionDoesNotRegressDeployedImage proves an out-of-order
// or redelivered older revision can neither overwrite a newer deployment's
// artifact and rollout nor supersede the newer revision's queued build.
func TestLateWebhookRevisionDoesNotRegressDeployedImage(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-1: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	image1 := testPinnedRef("registry.example.test/platform/web", "1")
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", image1, ""); err != nil {
		t.Fatalf("completeBuild commit-1: %v", err)
	}

	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-2: %v", err)
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// A redelivered older revision must not supersede the newer build
	// that is still queued.
	redelivered, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, source.BuildTransition{PreviousCommit: "commit-parent"})
	if err != nil || !redelivered.Superseded || redelivered.BuildID != "" || redelivered.DeploymentID != "" {
		t.Fatalf("redelivered older revision = %+v, %v, want superseded no-op", redelivered, err)
	}
	if build, err := store.reads.BuildByID(ctx, build2.ID); err != nil || build.State != deliverycore.BuildStateQueued {
		t.Fatalf("newer build = %+v, %v, want queued", build, err)
	}

	claimBuildForTest(t, store, ctx, "builder-1", build2.ID)
	image2 := testPinnedRef("registry.example.test/platform/web", "2")
	if err := completeBuildForTest(ctx, store, "builder-1", build2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-2", image2, ""); err != nil {
		t.Fatalf("completeBuild commit-2: %v", err)
	}

	// After commit-2 is deployed, its reuse of commit-1's image must not
	// roll the service back.
	redelivered, err = testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, source.BuildTransition{PreviousCommit: "commit-parent"})
	if err != nil || !redelivered.Superseded {
		t.Fatalf("redelivered older revision after deploy = %+v, %v, want superseded no-op", redelivered, err)
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID).ImageDigest; got != image2 {
		t.Fatalf("late revision regressed the deployment image to %q, want %q", got, image2)
	}
	artifacts, err := testDelivery(store).ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("artifacts = %v, %v; want exactly the two built images", artifacts, err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 2 {
		t.Fatalf("build_runs rows = %d, want 2 (no work for the late revision)", buildCount)
	}

	// An ordered push transition may deliberately move backward: a
	// force-push of commit-1 over commit-2 carries before=commit-2 and
	// proves its currency, so commit-1's artifact rolls out again.
	forced, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, source.BuildTransition{PreviousCommit: "commit-2"})
	if err != nil || !forced.Reused || forced.BuildID != build1.ID {
		t.Fatalf("force-push transition = %+v, %v, want reuse of %q", forced, err, build1.ID)
	}
	if got := currentDeploymentForTest(t, store, ctx, service.ID).ImageDigest; got != image1 {
		t.Fatalf("force-push rolled out %q, want the ordered target %q", got, image1)
	}
}

// TestEnqueueBuildSourceStateRejectsStaleTrackedHeadSync is the
// delayed-sync regression: a tracked-head sync that fetched before a
// newer push landed carries a stale fetch basis. It must neither move the
// proven head backward nor retire the newer revision's work — while a
// sync whose fetch basis is the current head still advances it.
func TestEnqueueBuildSourceStateRejectsStaleTrackedHeadSync(t *testing.T) {
	ctx := context.Background()
	store, _, service := newRepoBuildTestService(t)

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// The sync fetched commit-1 over no established head, and commit-2's
	// push advanced the binding before the sync applied. Its observation
	// is history only: it must not roll the head back or supersede work.
	stale := source.BuildTransition{TrackedHead: true}
	superseded, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, stale)
	if err != nil || !superseded.Superseded || superseded.PendingPredecessor {
		t.Fatalf("stale tracked-head sync = %+v, %v, want superseded no-op", superseded, err)
	}
	head, err := store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-2" {
		t.Fatalf("proven head = %q, %v; the stale sync must not move it backward", head, err)
	}

	// A sync whose fetch started over the current head is the real thing:
	// it advances the head to the fetched commit.
	fresh := source.BuildTransition{TrackedHead: true, FetchedFromHead: "commit-2"}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-3", "", "", fresh); err != nil {
		t.Fatalf("seedReadySourceState commit-3: %v", err)
	}
	head, err = store.source.SourceBindingHeadCommit(ctx, binding.ID)
	if err != nil || head != "commit-3" {
		t.Fatalf("proven head = %q, %v; a sync over the current head must advance it", head, err)
	}
}

// TestRedeliveredRevisionWithoutArtifactDoesNotSupersedeQueuedBuild is the
// late-webhook regression without a reusable image: a redelivered older
// revision whose build never produced an artifact must not retire the
// newer revision's queued build or queue stale work in its place.
func TestRedeliveredRevisionWithoutArtifactDoesNotSupersedeQueuedBuild(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-1: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "compile error"); err != nil {
		t.Fatalf("completeBuild commit-1: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-2: %v", err)
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	for _, transition := range []source.BuildTransition{
		{PreviousCommit: "commit-parent"},
		{},
	} {
		redelivered, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-1", source.SourceSnapshotRecord{}, transition)
		if err != nil || !redelivered.Superseded || redelivered.BuildID != "" || redelivered.DeploymentID != "" {
			t.Fatalf("redelivered older revision %+v = %+v, %v, want superseded no-op", transition, redelivered, err)
		}
	}
	if build, err := store.reads.BuildByID(ctx, build2.ID); err != nil || build.State != deliverycore.BuildStateQueued {
		t.Fatalf("newer build = %+v, %v, want queued", build, err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 2 {
		t.Fatalf("build_runs rows = %d, want 2 (no work for the late revision)", buildCount)
	}
}

// TestUnseenLateWebhookRevisionCannotPassTheFreshnessFence is the
// out-of-order regression for revisions that were never seen before: a
// delayed webhook for an older commit records it as history, but recording
// must not make it the freshest state. It may neither supersede the newer
// revision's queued build nor block the next real push, and it never
// demands build work of its own.
func TestUnseenLateWebhookRevisionCannotPassTheFreshnessFence(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-1: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_FAILED, "commit-1", "", "compile error"); err != nil {
		t.Fatalf("completeBuild commit-1: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-2: %v", err)
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// A delayed webhook for an older commit that was never observed before
	// records it with its push transition (before=commit-1). The
	// record is history only: it must not become the freshest state just
	// because it arrived last. Its push transition names commit-1 —
	// observed and no longer the proven head — so the staleness is
	// provable and the request is dropped outright, not requeued.
	lateTransition := source.BuildTransition{PreviousCommit: "commit-1"}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-late", "", "", lateTransition); err != nil {
		t.Fatalf("seedReadySourceState commit-late: %v", err)
	}
	redelivered, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-late", source.SourceSnapshotRecord{}, lateTransition)
	if err != nil || !redelivered.Superseded || redelivered.PendingPredecessor || redelivered.BuildID != "" || redelivered.DeploymentID != "" {
		t.Fatalf("unseen late revision = %+v, %v, want superseded no-op without requeue", redelivered, err)
	}
	if build, err := store.reads.BuildByID(ctx, build2.ID); err != nil || build.State != deliverycore.BuildStateQueued {
		t.Fatalf("newer build = %+v, %v, want queued", build, err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 2 {
		t.Fatalf("build_runs rows = %d, want 2 (no work for the late revision)", buildCount)
	}

	// The next real push (before=commit-2) lands even though the stale
	// observation arrived after it: a refused observation must not fence
	// out the tracked ref's actual successor.
	nextTransition := source.BuildTransition{PreviousCommit: "commit-2"}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-3", "", "", nextTransition); err != nil {
		t.Fatalf("seedReadySourceState commit-3: %v", err)
	}
	next, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-3", source.SourceSnapshotRecord{}, nextTransition)
	if err != nil || next.Superseded || next.BuildID == "" {
		t.Fatalf("next real push = %+v, %v, want queued build", next, err)
	}
}

// TestSuccessorPushBeforePredecessorIsRequeuedThenBuilt: a webhook for a
// successor push — its transition names a commit the binding has not
// observed yet — can arrive before its predecessor's webhook is
// processed. That request is early, not stale, and dropping it would lose
// the push forever: the work item completes and nobody retries the
// successor. QueueSourceBuild reports PendingPredecessor so the work
// loop requeues instead of completing, and once the predecessor lands
// and advances the proven head, the successor proves currency and builds.
func TestSuccessorPushBeforePredecessorIsRequeuedThenBuilt(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	binding, err := store.source.SourceBindingByServiceID(ctx, service.ID)
	if err != nil {
		t.Fatalf("SourceBindingByServiceID: %v", err)
	}

	// The successor's webhook lands first: its push transition names
	// commit-mid, which is not observed yet.
	earlyTransition := source.BuildTransition{PreviousCommit: "commit-mid"}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-successor", "", "", earlyTransition); err != nil {
		t.Fatalf("seedReadySourceState commit-successor: %v", err)
	}
	early, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-successor", source.SourceSnapshotRecord{}, earlyTransition)
	if err != nil || !early.Superseded || !early.PendingPredecessor || early.BuildID != "" {
		t.Fatalf("early successor = %+v, %v, want superseded with pending predecessor", early, err)
	}
	var buildCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_runs WHERE service_id = $1`, service.ID).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 0 {
		t.Fatalf("build_runs rows = %d, want 0 (the early successor creates no work yet)", buildCount)
	}

	// The predecessor lands and advances the proven head.
	midTransition := source.BuildTransition{PreviousCommit: "commit-1"}
	if err := seedReadySourceStateWithMetadata(t, store, service, "commit-mid", "", "", midTransition); err != nil {
		t.Fatalf("seedReadySourceState commit-mid: %v", err)
	}
	mid, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-mid", source.SourceSnapshotRecord{}, midTransition)
	if err != nil || mid.Superseded || mid.BuildID == "" {
		t.Fatalf("predecessor = %+v, %v, want queued build", mid, err)
	}

	// The requeued successor now proves currency (before == head) and
	// builds: the out-of-order pair converges on the real ref tip.
	requeued, err := testDelivery(store).QueueSourceBuild(ctx, binding, "commit-successor", source.SourceSnapshotRecord{}, earlyTransition)
	if err != nil || requeued.Superseded || requeued.BuildID == "" {
		t.Fatalf("requeued successor = %+v, %v, want queued build", requeued, err)
	}
}

func seedArtifactForRetentionTest(t *testing.T, store *persistence, ctx context.Context, serviceID, id, nibble string, created time.Time) {
	t.Helper()
	digest := testDigest(nibble)
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO build_artifacts(id, service_id, build_id, kind, source_snapshot_digest, commit_sha,
			build_recipe_json, builder_version, image_repository, image_manifest_digest,
			image_ref, source_image_ref, reuse_key, build_actor_kind, build_actor_id, created_at)
		VALUES ($1, $2, NULL, 'direct_image', '', '', '{}', '', 'example.test/web', $3,
			$4, '', '', 'user', 'user-1', $5)`,
		id, serviceID, digest, "example.test/web@"+digest, created); err != nil {
		t.Fatal(err)
	}
}

// TestSupersededBuildArtifactAgesOutWithRetention proves a build that
// finishes after newer work was queued does not pin its never-deployed
// image as rollback material: the superseded deployment keeps its
// history, but retention may age the undeployed artifact out.
func TestSupersededBuildArtifactAgesOutWithRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, service := newRepoBuildTestService(t)

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-1: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	if _, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2"); err != nil {
		t.Fatalf("enqueueBuildForTest commit-2: %v", err)
	}
	// Stand in for the in-flight window the finding describes: the older
	// build's deployment is still the current one when its late completion
	// lands after newer work was queued.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE deployments SET is_current = FALSE WHERE service_id = $1 AND build_id <> $2`, service.ID, build1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE deployments SET is_current = TRUE, state = 'queued_build' WHERE build_id = $1`, build1.ID); err != nil {
		t.Fatal(err)
	}
	image1 := testPinnedRef("registry.example.test/platform/web", "1")
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", image1, ""); err != nil {
		t.Fatalf("completeBuild commit-1: %v", err)
	}

	var artifactID string
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT artifact_id FROM build_runs WHERE id = $1), '')`, build1.ID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	if artifactID == "" {
		t.Fatal("completed build recorded no artifact")
	}
	var pinned string
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT COALESCE(artifact_id, '') FROM deployments WHERE build_id = $1 LIMIT 1), 'missing')`, build1.ID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != "" {
		t.Fatalf("superseded deployment pinned artifact %q; never-deployed images are not rollback material", pinned)
	}
	var depState string
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT state FROM deployments WHERE build_id = $1 LIMIT 1), '')`, build1.ID).Scan(&depState); err != nil {
		t.Fatal(err)
	}
	if depState != "superseded" {
		t.Fatalf("build1 deployment state = %q, want the doomed build's history entry kept as superseded", depState)
	}
	deleted, err := testDelivery(store).PruneBuildArtifacts(ctx, time.Now().UTC().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("PruneBuildArtifacts: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("pruned %d artifacts, want the superseded build's undeployed image to age out", deleted)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM build_artifacts WHERE id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("a never-deployed artifact evaded retention by pinning itself to its superseded deployment")
	}
}

// TestPruneBuildArtifactsKeepsRollbackMaterial proves retention: artifacts
// any deployment, transition, rollout, or current pointer references
// survive any age, recent artifacts are kept, and only aged-out
// unreferenced artifacts beyond the newest keep-recent are deleted.
func TestPruneBuildArtifactsKeepsRollbackMaterial(t *testing.T) {
	t.Parallel()
	store, ctx, _, _, service := setupPinnedImageServiceForDeployment(t, pinnedImage("a"))
	now := time.Now().UTC()

	seedArtifactForRetentionTest(t, store, ctx, service.ID, "artifact-old-first", "b", now.AddDate(0, 0, -42))
	seedArtifactForRetentionTest(t, store, ctx, service.ID, "artifact-old-second", "c", now.AddDate(0, 0, -41))
	seedArtifactForRetentionTest(t, store, ctx, service.ID, "artifact-old-newest", "d", now.AddDate(0, 0, -40))
	seedArtifactForRetentionTest(t, store, ctx, service.ID, "artifact-old-referenced", "e", now.AddDate(0, 0, -43))
	seedArtifactForRetentionTest(t, store, ctx, service.ID, "artifact-recent", "f", now.AddDate(0, 0, -1))
	if _, err := store.db.ExecContext(ctx,
		`UPDATE service_delivery_status SET current_artifact_id = $1 WHERE service_id = $2`,
		"artifact-old-referenced", service.ID); err != nil {
		t.Fatal(err)
	}

	deleted, err := testDelivery(store).PruneBuildArtifacts(ctx, now.AddDate(0, 0, -30), 1)
	if err != nil {
		t.Fatalf("PruneBuildArtifacts: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("pruned %d artifacts, want the 2 oldest unreferenced", deleted)
	}

	rows, err := store.db.QueryContext(ctx, `SELECT id FROM build_artifacts WHERE service_id = $1`, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	remaining := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"artifact-old-newest":     true, // newest unreferenced old artifact kept per keep-recent
		"artifact-old-referenced": true, // referenced by the current pointer: rollback material
		"artifact-recent":         true, // inside the retention window
	}
	gone := []string{"artifact-old-first", "artifact-old-second"}
	for id := range want {
		if !remaining[id] {
			t.Fatalf("retention deleted %s; remaining = %v", id, remaining)
		}
	}
	for _, id := range gone {
		if remaining[id] {
			t.Fatalf("retention kept unreferenced aged artifact %s; remaining = %v", id, remaining)
		}
	}
}

// TestRepeatedBuildsOfSameImageKeepOwnProvenance: when a later build of a
// different revision reports the same manifest digest, each successful
// build keeps its own artifact row so provenance (commit, build, actor)
// stays with the build that produced the image instead of collapsing into
// the first artifact recorded for that image.
func TestRepeatedBuildsOfSameImageKeepOwnProvenance(t *testing.T) {
	t.Parallel()
	store, _, service := newRepoBuildTestService(t)
	ctx := context.Background()

	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatalf("seedReadySourceState commit-1: %v", err)
	}
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-1: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build1.ID)
	image := testPinnedRef("registry.example.test/platform/web", "9")
	if err := completeBuildForTest(ctx, store, "builder-1", build1.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-1", image, ""); err != nil {
		t.Fatalf("completeBuild commit-1: %v", err)
	}
	if err := seedReadySourceState(t, store, service, "commit-2"); err != nil {
		t.Fatalf("seedReadySourceState commit-2: %v", err)
	}
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueueBuildForTest commit-2: %v", err)
	}
	claimBuildForTest(t, store, ctx, "builder-1", build2.ID)
	if err := completeBuildForTest(ctx, store, "builder-1", build2.ID, platformv1.BuildState_BUILD_STATE_SUCCEEDED, "commit-2", image, ""); err != nil {
		t.Fatalf("completeBuild commit-2: %v", err)
	}

	artifacts, err := testDelivery(store).ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil {
		t.Fatalf("ListServiceArtifacts: %v", err)
	}
	byBuild := map[string]deliverycore.BuildArtifactRecord{}
	for _, artifact := range artifacts {
		byBuild[artifact.BuildID] = artifact
	}
	if len(artifacts) != 2 || len(byBuild) != 2 {
		t.Fatalf("artifacts = %+v, want one artifact per successful build", artifacts)
	}
	first, second := byBuild[build1.ID], byBuild[build2.ID]
	if first.ID == second.ID {
		t.Fatalf("both builds share artifact %s, want distinct rows", first.ID)
	}
	if first.ImageRef != image || second.ImageRef != image {
		t.Fatalf("artifact images = (%q, %q), want %q", first.ImageRef, second.ImageRef, image)
	}
	if first.CommitSHA != "commit-1" || second.CommitSHA != "commit-2" {
		t.Fatalf("artifact provenance = (%q, %q), want (commit-1, commit-2)", first.CommitSHA, second.CommitSHA)
	}
	completed, err := store.reads.BuildByID(ctx, build2.ID)
	if err != nil {
		t.Fatalf("BuildByID: %v", err)
	}
	if completed.ArtifactID != second.ID {
		t.Fatalf("second build artifact = %q, want its own artifact %q (not the earlier %q)", completed.ArtifactID, second.ID, first.ID)
	}
}

// TestDirectImagePinnedInputRecordsNoMutableSourceRef pins the proto
// contract for source_image_ref: it keeps the user's original input only
// when that input was not digest-pinned. An already-pinned reference must
// not round-trip to clients as mutable user input.
func TestDirectImagePinnedInputRecordsNoMutableSourceRef(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.catalog.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.catalog.listProjects(ctx, testUser("user-1"), false)
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	pinned := testPinnedRef("example.test/web", "b")
	delivery := newTestDelivery(store, nil, nil, nil)
	service, err := delivery.CreateService(ctx, testUser("user-1"), productionEnvironmentID(t, store, projects[0].ID), "web",
		directImageServiceSpec(pinned, &platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080})}), "node-1")
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	artifacts, err := delivery.ListServiceArtifacts(ctx, testUser("user-1"), service.ID, 10)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts after create = %v, %v", artifacts, err)
	}
	if artifacts[0].Kind != deliverycore.BuildArtifactDirectImage || artifacts[0].ImageRef != pinned {
		t.Fatalf("direct-image artifact = %+v", artifacts[0])
	}
	if artifacts[0].SourceImageRef != "" {
		t.Fatalf("digest-pinned input must not be recorded as mutable user input, got %q", artifacts[0].SourceImageRef)
	}
}
