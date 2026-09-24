package delivery

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestArtifactReuseKey(t *testing.T) {
	t.Parallel()

	dockerfile := &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "Dockerfile", ContextDir: "."}
	implicitDefaults := &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE}
	railpack := &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK}

	base := artifactReuseKey("sha256:snapshot", dockerfile)
	if base == "" {
		t.Fatal("reuse key is empty")
	}
	if got := artifactReuseKey("sha256:snapshot", implicitDefaults); got != base {
		t.Fatal("recipes with implicit defaults must converge on one reuse key")
	}
	if got := artifactReuseKey("sha256:other", dockerfile); got == base {
		t.Fatal("different snapshots must not share a reuse key")
	}
	if got := artifactReuseKey("sha256:snapshot", railpack); got == base {
		t.Fatal("different builders must not share a reuse key")
	}
	custom := &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE, DockerfilePath: "docker/Dockerfile", ContextDir: "."}
	if got := artifactReuseKey("sha256:snapshot", custom); got == base {
		t.Fatal("different recipes must not share a reuse key")
	}
}
