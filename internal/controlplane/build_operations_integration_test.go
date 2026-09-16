//go:build integration

package controlplane

import (
	"context"
	logscore "ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"errors"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retryBuildCredentials struct{ attempts int }

func (c *retryBuildCredentials) CredentialsForBuild(context.Context, string, string, string) (string, string, error) {
	c.attempts++
	if c.attempts == 1 {
		return "", "", errors.New("credentials temporarily unavailable")
	}
	return "builder", "password", nil
}

func TestBuildOperationsRetriesPreparationWithoutLosingLease(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	project, err := store.catalog.createProject(ctx, testUser("owner"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createScheduledService(ctx, store, "owner", productionEnvironmentID(t, store, project.ID), "web", repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		Provider: "github", RepositorySelector: "octocat/hello", TrackedRef: "main", BuildRecipe: &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := seedReadySourceState(t, store, service, "commit-1"); err != nil {
		t.Fatal(err)
	}
	build, err := enqueueBuildForTest(ctx, store, "owner", service.ID, "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	policy := registry.NewPolicy(config.RegistryConfig{Host: "registry.example.test", NamespacePrefix: "platform", CredentialTTLSeconds: 300}, nil)
	credentials := &retryBuildCredentials{}
	notifier := &recordingNotifier{}
	logWriter := &recordingLogWriter{enabled: true}
	operations := NewBuildOperations(store.builds, store.reads, store.source, newDelivery(store, notifier, nil, nil, nil), policy, credentials, 0, WithBuilderLogEmitter(logscore.NewLogEmitter(logWriter)))
	builder := contextWithClientIdentity(serviceCallerBuilder, "builder-1")
	claim := &platformv1.ClaimBuildRequest{BuilderId: "builder-1"}
	if _, err := operations.ClaimBuild(builder, claim); err == nil {
		t.Fatal("expected credential preparation failure")
	}
	job, err := operations.ClaimBuild(builder, claim)
	if err != nil || job.GetBuildId() != build.ID || job.GetRegistryPassword() != "password" {
		t.Fatalf("retry lost prepared job: %v, %v", job, err)
	}
	if job.GetSource().GetBuildRecipe().GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE {
		t.Fatalf("claimed job lost builder choice: %+v", job.GetSource().GetBuildRecipe())
	}
	image := policy.RuntimeDigestRef(job.RegistryPushReference, "sha256:"+strings.Repeat("a", 64))
	completion := &platformv1.CompleteBuildRequest{BuilderId: "builder-1", BuildId: build.ID, State: platformv1.BuildState_BUILD_STATE_SUCCEEDED, CommitSha: "wrong-commit", ImageDigest: image}
	if _, err := operations.CompleteBuild(builder, completion); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mismatched commit accepted: %v", err)
	}
	completion.CommitSha = "commit-1"
	completion.ImageDigest = "other.example/repository@sha256:" + strings.Repeat("a", 64)
	if _, err := operations.CompleteBuild(builder, completion); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("foreign repository accepted: %v", err)
	}
	if len(notifier.agentIDs) != 0 {
		t.Fatal("invalid completion woke agents")
	}
	completion.ImageDigest = image
	if _, err := operations.CompleteBuild(builder, completion); err != nil {
		t.Fatal(err)
	}
	beforeLogs, beforeWakes := len(logWriter.Flatten()), len(notifier.agentIDs)
	if beforeWakes == 0 {
		t.Fatal("completion did not wake allocated agent")
	}
	if _, err := operations.CompleteBuild(builder, completion); err != nil {
		t.Fatal(err)
	}
	if len(logWriter.Flatten()) != beforeLogs || len(notifier.agentIDs) != beforeWakes {
		t.Fatal("repeated completion repeated post-commit work")
	}
}
