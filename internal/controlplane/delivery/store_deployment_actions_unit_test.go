package delivery

import (
	"strings"
	"testing"
)

func TestImmutableImageReference(t *testing.T) {
	t.Parallel()
	digest := "example.test/web@sha256:" + strings.Repeat("ab", 32)
	if !immutableImageReference(digest) {
		t.Fatalf("expected digest-pinned image %q", digest)
	}
	for _, image := range []string{"", "nginx:latest", "example.test/web@sha256:deadbeef", "example.test/web@sha256:" + strings.Repeat("zz", 32)} {
		if immutableImageReference(image) {
			t.Fatalf("unexpected digest-pinned image %q", image)
		}
	}
}

func TestDeploymentReusableForRollback(t *testing.T) {
	t.Parallel()
	if !deploymentReusableForRollback(DeploymentStateDraining) || !deploymentReusableForRollback(DeploymentStateCompleted) || !deploymentReusableForRollback(DeploymentStateRemoved) {
		t.Fatal("expected previously successful deployments to be rollback sources")
	}
	if deploymentReusableForRollback(DeploymentStateFailed) || deploymentReusableForRollback(DeploymentStateCancelled) {
		t.Fatal("failed/cancelled deployments must not be rollback sources")
	}
}
