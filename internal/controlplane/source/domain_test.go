package source

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestBuildRecipeJSONRoundTripsBuilder(t *testing.T) {
	t.Parallel()

	raw, err := MarshalBuildRecipe(&platformv1.BuildRecipe{
		Builder:    platformv1.BuilderKind_BUILDER_KIND_RAILPACK,
		ContextDir: "apps/web",
	})
	if err != nil {
		t.Fatalf("MarshalBuildRecipe: %v", err)
	}
	recipe, err := UnmarshalBuildRecipe(raw)
	if err != nil {
		t.Fatalf("UnmarshalBuildRecipe: %v", err)
	}
	if recipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK || recipe.GetContextDir() != "apps/web" {
		t.Fatalf("unexpected round-tripped recipe %+v", recipe)
	}
}

func TestBuildRecipeJSONRoundTripsDockerfile(t *testing.T) {
	t.Parallel()

	raw, err := MarshalBuildRecipe(&platformv1.BuildRecipe{
		Builder:        platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE,
		DockerfilePath: "deploy/Dockerfile",
		ContextDir:     "deploy",
	})
	if err != nil {
		t.Fatalf("MarshalBuildRecipe: %v", err)
	}
	recipe, err := UnmarshalBuildRecipe(raw)
	if err != nil {
		t.Fatalf("UnmarshalBuildRecipe: %v", err)
	}
	if recipe.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE ||
		recipe.GetDockerfilePath() != "deploy/Dockerfile" ||
		recipe.GetContextDir() != "deploy" {
		t.Fatalf("unexpected round-tripped recipe %+v", recipe)
	}
}
