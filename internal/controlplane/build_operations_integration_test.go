//go:build integration

package controlplane

import (
	"context"
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
	project, err := store.createProject(ctx, "owner", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertTestAgent(t, store, ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}
	service, err := createScheduledService(ctx, store, "owner", productionEnvironmentID(t, store, project.ID), "web", repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		Provider: "github", RepositorySelector: "octocat/hello", TrackedRef: "main", BuildRecipe: &platformv1.BuildRecipe{DockerfilePath: "Dockerfile", ContextDir: "."},
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
	registry := NewRegistryPolicy(config.RegistryConfig{Host: "registry.example.test", NamespacePrefix: "platform", CredentialTTLSeconds: 300})
	credentials := &retryBuildCredentials{}
	notifier := &recordingNotifier{}
	logs := &recordingLogWriter{enabled: true}
	operations := NewBuildOperations(store, NewDelivery(store, notifier, nil, nil), registry, credentials, 0, WithBuilderLogEmitter(&LogEmitter{store: logs}))
	builder := contextWithClientIdentity(serviceCallerBuilder, "builder-1")
	claim := &platformv1.ClaimBuildRequest{BuilderId: "builder-1"}
	if _, err := operations.ClaimBuild(builder, claim); err == nil {
		t.Fatal("expected credential preparation failure")
	}
	job, err := operations.ClaimBuild(builder, claim)
	if err != nil || job.GetBuildId() != build.ID || job.GetRegistryPassword() != "password" {
		t.Fatalf("retry lost prepared job: %v, %v", job, err)
	}
	image := registry.RuntimeDigestRef(job.RegistryPushReference, "sha256:"+strings.Repeat("a", 64))
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
	beforeLogs, beforeWakes := len(logs.Flatten()), len(notifier.agentIDs)
	if beforeWakes == 0 {
		t.Fatal("completion did not wake allocated agent")
	}
	if _, err := operations.CompleteBuild(builder, completion); err != nil {
		t.Fatal(err)
	}
	if len(logs.Flatten()) != beforeLogs || len(notifier.agentIDs) != beforeWakes {
		t.Fatal("repeated completion repeated post-commit work")
	}
}
