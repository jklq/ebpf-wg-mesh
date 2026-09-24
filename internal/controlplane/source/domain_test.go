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

func TestBuildTransitionProvesCurrent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		transition BuildTransition
		revision   string
		head       string
		want       bool
	}{
		{name: "no head establishes it", transition: BuildTransition{PreviousCommit: "p"}, revision: "c", head: "", want: true},
		{name: "redelivery of the head", transition: BuildTransition{}, revision: "c", head: "c", want: true},
		{name: "push advances from the head", transition: BuildTransition{PreviousCommit: "h"}, revision: "c", head: "h", want: true},
		{name: "push chains to another predecessor", transition: BuildTransition{PreviousCommit: "x"}, revision: "c", head: "h", want: false},
		{name: "unproven request against an existing head", transition: BuildTransition{}, revision: "c", head: "h", want: false},
		{name: "fetch over the current head", transition: BuildTransition{TrackedHead: true, FetchedFromHead: "h"}, revision: "c", head: "h", want: true},
		{name: "delayed fetch over a head a push replaced", transition: BuildTransition{TrackedHead: true, FetchedFromHead: "x"}, revision: "c", head: "h", want: false},
		{name: "delayed fetch from before any head", transition: BuildTransition{TrackedHead: true}, revision: "c", head: "h", want: false},
		{name: "history never proves", transition: BuildTransition{History: true, PreviousCommit: "h"}, revision: "c", head: "h", want: false},
		{name: "history never establishes an empty head", transition: BuildTransition{History: true}, revision: "c", head: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.transition.ProvesCurrent(tc.revision, tc.head); got != tc.want {
				t.Fatalf("ProvesCurrent(%q, %q) = %v, want %v", tc.revision, tc.head, got, tc.want)
			}
		})
	}
}

func TestNoPushPredecessor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sha  string
		want bool
	}{
		{sha: "0000000000000000000000000000000000000000", want: true},
		{sha: "0000000000000000000000000000000000000000000000000000000000000000", want: true},
		{sha: " 0000000000000000000000000000000000000000 ", want: true},
		{sha: "", want: true},
		{sha: "   ", want: true},
		{sha: "commit-parent", want: false},
		{sha: "00000000000000000000000000000000000000a", want: false},
	} {
		if got := NoPushPredecessor(tc.sha); got != tc.want {
			t.Fatalf("NoPushPredecessor(%q) = %v, want %v", tc.sha, got, tc.want)
		}
	}
}
