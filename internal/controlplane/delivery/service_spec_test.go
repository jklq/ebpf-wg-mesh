package delivery

import (
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestValidateBuildRecipe(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		spec    *platformv1.ServiceSpec
		wantErr bool
	}{
		{
			name:    "direct image needs no recipe",
			spec:    directImageServiceSpec("repo/app:v1", nil),
			wantErr: false,
		},
		{
			name: "railpack recipe",
			spec: repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "owner/repo",
				BuildRecipe: &platformv1.BuildRecipe{
					Builder:    platformv1.BuilderKind_BUILDER_KIND_RAILPACK,
					ContextDir: ".",
				},
			}),
			wantErr: false,
		},
		{
			name: "dockerfile recipe",
			spec: repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "owner/repo",
				BuildRecipe: &platformv1.BuildRecipe{
					Builder:        platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE,
					DockerfilePath: "Dockerfile",
					ContextDir:     ".",
				},
			}),
			wantErr: false,
		},
		{
			name: "missing recipe",
			spec: repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "owner/repo",
			}),
			wantErr: true,
		},
		{
			name: "unspecified builder",
			spec: repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
				Provider:           "github",
				RepositorySelector: "owner/repo",
				BuildRecipe: &platformv1.BuildRecipe{
					DockerfilePath: "Dockerfile",
					ContextDir:     ".",
				},
			}),
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateBuildRecipe(CanonicalServiceSpec(test.spec))
			if test.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestCanonicalServiceSpecBuildRecipeDefaults(t *testing.T) {
	t.Parallel()

	dockerfile := CanonicalServiceSpec(repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		BuildRecipe: &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE},
	})).GetSource().GetSourceSpec().GetBuildRecipe()
	if dockerfile.GetDockerfilePath() != "Dockerfile" || dockerfile.GetContextDir() != "." {
		t.Fatalf("unexpected dockerfile defaults %+v", dockerfile)
	}

	railpack := CanonicalServiceSpec(repositoryServiceSpec(nil, &platformv1.ServiceSourceSpec{
		BuildRecipe: &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK},
	})).GetSource().GetSourceSpec().GetBuildRecipe()
	if railpack.GetDockerfilePath() != "" || railpack.GetContextDir() != "." {
		t.Fatalf("unexpected railpack defaults %+v", railpack)
	}
}

func TestToProtoBuildStatusCarriesBuilder(t *testing.T) {
	t.Parallel()

	status := ToProtoBuildStatus(BuildRunRecord{
		ID:          "build-1",
		State:       BuildStateRunning,
		BuildRecipe: &platformv1.BuildRecipe{Builder: platformv1.BuilderKind_BUILDER_KIND_RAILPACK},
	})
	if status.GetBuilder() != platformv1.BuilderKind_BUILDER_KIND_RAILPACK {
		t.Fatalf("unexpected build status builder %v", status.GetBuilder())
	}
}

func TestEqualDesiredSourceSpecComparesBuilder(t *testing.T) {
	t.Parallel()

	recipe := func(builder platformv1.BuilderKind) *platformv1.ServiceSourceSpec {
		return &platformv1.ServiceSourceSpec{
			Provider:           "github",
			RepositorySelector: "owner/repo",
			TrackedRef:         "main",
			BuildRecipe: &platformv1.BuildRecipe{
				Builder:        builder,
				DockerfilePath: "Dockerfile",
				ContextDir:     ".",
			},
		}
	}
	if !equalDesiredSourceSpec(recipe(platformv1.BuilderKind_BUILDER_KIND_RAILPACK), recipe(platformv1.BuilderKind_BUILDER_KIND_RAILPACK)) {
		t.Fatal("identical railpack recipes compared unequal")
	}
	if equalDesiredSourceSpec(recipe(platformv1.BuilderKind_BUILDER_KIND_RAILPACK), recipe(platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE)) {
		t.Fatal("railpack and dockerfile recipes compared equal")
	}
}
