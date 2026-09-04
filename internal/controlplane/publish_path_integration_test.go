//go:build integration

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/testutil"
)

func TestConnectedPublishSnapshotBecomesDesiredDigest(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		bootstrap: config.BootstrapConfig{
			Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
		},
		registry:      localRegistryConfig(t),
		withDashboard: true,
	})
	store := cp.server.store
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080}), CpuMillis: 250, MemoryMebibytes: 256},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}
	marker := fmt.Sprintf("publish-%d", time.Now().UnixNano())
	seedDockerfileSourceState(t, store, service, "commit-1", marker)

	before, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.GetServices()) != 0 {
		t.Fatalf("desired state had %d services before build completion", len(before.GetServices()))
	}

	build, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueueBuildForTest: %v", err)
	}
	startTestBuilder(t, cp.server, "builder-publish")

	var desiredImage string
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 50 * time.Second, Interval: 250 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		state, err := store.desiredStateForAgent(ctx, "node-1")
		if err != nil {
			return false, err
		}
		if len(state.GetServices()) != 1 {
			return false, nil
		}
		desiredImage = state.GetServices()[0].GetSpec().GetImage()
		return desiredImage != "", nil
	}); err != nil {
		t.Fatalf("wait for desired digest: %v", err)
	}
	if !strings.Contains(desiredImage, "@sha256:") {
		t.Fatalf("desired image is not a digest: %q", desiredImage)
	}
	if strings.Contains(desiredImage, ":git-") && !strings.Contains(desiredImage, "@sha256:") {
		t.Fatalf("desired image used a mutable tag: %q", desiredImage)
	}

	completed, err := store.buildRunByIDQuerier(ctx, store.db, build.ID)
	if err != nil {
		t.Fatalf("load completed build: %v", err)
	}
	if completed.State != buildStateSucceeded || completed.ImageDigest != desiredImage {
		t.Fatalf("build record %+v does not match desired image %q", completed, desiredImage)
	}

	desired := mustDesiredService(t, store, "node-1")
	username, password, err := cp.server.registry.CredentialsForPull("node-1-"+desired.GetAllocationId(), desired.GetEnvironmentId(), desired.GetServiceId(), desiredImage)
	if err != nil || username == "" {
		t.Fatalf("CredentialsForPull: %q %q %v", username, password, err)
	}
	authAddr := cp.server.RegistryAuthAddr()
	if status := registryManifestStatus(t, authAddr, cp.cfg.Registry, username, password, desiredImage); status != http.StatusOK {
		t.Fatalf("agent pull credential could not fetch digest: status %d", status)
	}

	otherUser, otherPass, err := cp.server.registryAuth.MintCredential("pull-other", "mesh/other-env/other-build/other-svc", []string{"pull"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status := registryManifestStatus(t, authAddr, cp.cfg.Registry, otherUser, otherPass, desiredImage); status != http.StatusUnauthorized {
		t.Fatalf("foreign credential status = %d, want 401", status)
	}

	userCtx := userContext(t, ctx, "user-1")
	deployments, err := cp.dashboard.ListServiceDeployments(userCtx, &platformv1.ListServiceDeploymentsRequest{ServiceId: service.ID})
	if err != nil {
		t.Fatalf("ListServiceDeployments: %v", err)
	}
	current, err := store.serviceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deploymentLinksDigest(deployments.GetDeployments(), current.RolloutGeneration, desiredImage) {
		t.Fatalf("deployments did not link digest %s to rollout %d: %+v", desiredImage, current.RolloutGeneration, deployments.GetDeployments())
	}
}

func TestConnectedPublishLateCompleteCannotStealDesiredDigest(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cp := startSystemControlPlane(t, systemControlPlaneOptions{
		bootstrap: config.BootstrapConfig{
			Users: []config.BootstrapUser{{ID: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
		},
		registry:      localRegistryConfig(t),
		withDashboard: true,
	})
	store := cp.server.store
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	service, err := store.createService(ctx, "user-1", productionEnvironmentID(t, store, projects[0].ID), "web", repositoryServiceSpec(
		&platformv1.ServiceRuntime{Ports: runtimePortsFromInts([]int32{8080}), CpuMillis: 250, MemoryMebibytes: 256},
		&platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "octocat/hello",
			TrackedRef:         "main",
			BuildRecipe:        &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
		},
	), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	seedDockerfileSourceState(t, store, service, "commit-1", "late-1")
	build1, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-1")
	if err != nil {
		t.Fatalf("enqueue build 1: %v", err)
	}
	late := dialBuilderClient(t, cp.server, "builder-late")
	job1, err := late.ClaimBuild(ctx, &platformv1.ClaimBuildRequest{BuilderId: "builder-late", BuilderName: "late"})
	if err != nil || job1.GetBuildId() != build1.ID {
		t.Fatalf("claim build 1: %+v %v", job1, err)
	}

	seedDockerfileSourceState(t, store, service, "commit-2", "late-2")
	build2, err := enqueueBuildForTest(ctx, store, "user-1", service.ID, "commit-2")
	if err != nil {
		t.Fatalf("enqueue build 2: %v", err)
	}
	startTestBuilder(t, cp.server, "builder-win")

	var winner string
	if err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 50 * time.Second, Interval: 250 * time.Millisecond}, func(ctx context.Context) (bool, error) {
		state, err := store.desiredStateForAgent(ctx, "node-1")
		if err != nil {
			return false, err
		}
		if len(state.GetServices()) != 1 {
			return false, nil
		}
		winner = state.GetServices()[0].GetSpec().GetImage()
		return winner != "", nil
	}); err != nil {
		t.Fatalf("wait for build 2 digest: %v", err)
	}
	completed2, err := store.buildRunByIDQuerier(ctx, store.db, build2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed2.ImageDigest != winner {
		t.Fatalf("winner image %q does not match build 2 %q", winner, completed2.ImageDigest)
	}

	lateDigest := cp.server.registry.RuntimeDigestRef(job1.GetRegistryPushReference(), "sha256:"+strings.Repeat("a", 64))
	if _, err := late.CompleteBuild(ctx, &platformv1.CompleteBuildRequest{
		BuilderId:   "builder-late",
		BuildId:     build1.ID,
		State:       platformv1.BuildState_BUILD_STATE_SUCCEEDED,
		CommitSha:   "commit-1",
		ImageDigest: lateDigest,
	}); err != nil {
		t.Fatalf("late CompleteBuild: %v", err)
	}
	after, err := store.desiredStateForAgent(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := after.GetServices()[0].GetSpec().GetImage(); got != winner {
		t.Fatalf("late complete stole desired image: got %q want %q", got, winner)
	}
	current, err := store.serviceByID(ctx, "user-1", service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ResolvedImage != winner {
		t.Fatalf("resolved image after late complete = %q, want %q", current.ResolvedImage, winner)
	}

	build2Repo, err := cp.server.registry.repositoryForReference(winner)
	if err != nil {
		t.Fatal(err)
	}
	if status := registryTagsStatus(t, cp.server.RegistryAuthAddr(), cp.cfg.Registry, job1.GetRegistryUsername(), job1.GetRegistryPassword(), build2Repo); status != http.StatusUnauthorized {
		t.Fatalf("build 1 credential accessed build 2 repo: status %d", status)
	}
}

func mustDesiredService(t *testing.T, store *Store, agentID string) *agentv1.DesiredService {
	t.Helper()
	state, err := store.desiredStateForAgent(context.Background(), agentID)
	if err != nil || len(state.GetServices()) != 1 {
		t.Fatalf("desired service: %+v %v", state, err)
	}
	return state.GetServices()[0]
}

func deploymentLinksDigest(items []*platformv1.DeploymentRecord, generation int64, digest string) bool {
	for _, item := range items {
		linked := item.GetBuild().GetImageDigest() == digest || item.GetImageDigest() == digest
		if linked && item.GetRolloutGeneration() == generation {
			return true
		}
	}
	return false
}

func registryManifestStatus(t *testing.T, authAddr string, cfg config.RegistryConfig, username, password, imageRef string) int {
	t.Helper()
	repo, digest := splitImageDigest(t, cfg.Host, imageRef)
	token := requestRegistryToken(t, authAddr, cfg, username, password, "repository:"+repo+":pull")
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.Host+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func registryTagsStatus(t *testing.T, authAddr string, cfg config.RegistryConfig, username, password, repository string) int {
	t.Helper()
	token := requestRegistryToken(t, authAddr, cfg, username, password, "repository:"+repository+":pull")
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.Host+"/v2/"+repository+"/tags/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func requestRegistryToken(t *testing.T, authAddr string, cfg config.RegistryConfig, username, password, scope string) string {
	t.Helper()
	query := url.Values{"service": {cfg.TokenService}, "scope": {scope}}
	req, err := http.NewRequest(http.MethodGet, "http://"+authAddr+RegistryTokenPath+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Token
}

func splitImageDigest(t *testing.T, host, imageRef string) (string, string) {
	t.Helper()
	prefix := host + "/"
	if !strings.HasPrefix(imageRef, prefix) {
		t.Fatalf("image %q is outside %q", imageRef, host)
	}
	rest := strings.TrimPrefix(imageRef, prefix)
	at := strings.LastIndexByte(rest, '@')
	if at < 0 {
		t.Fatalf("image %q has no digest", imageRef)
	}
	return rest[:at], rest[at+1:]
}
