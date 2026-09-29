package delivery

import (
	"errors"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/proto"
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

const (
	defaultServiceCPUMillis       int64 = 250
	defaultServiceMemoryMebibytes int64 = 256
)

func directImageServiceSpec(image string, runtime *platformv1.ServiceRuntime) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{Image: image},
			},
		},
	}
}

func repositoryServiceSpec(runtime *platformv1.ServiceRuntime, source *platformv1.ServiceSourceSpec) *platformv1.ServiceSpec {
	if runtime == nil {
		runtime = defaultServiceRuntime()
	}
	if source == nil {
		source = &platformv1.ServiceSourceSpec{}
	}
	return &platformv1.ServiceSpec{
		Runtime: runtime,
		Source: &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_SourceSpec{
				SourceSpec: source,
			},
		},
	}
}

func defaultServiceRuntime() *platformv1.ServiceRuntime {
	return &platformv1.ServiceRuntime{
		CpuMillis:       defaultServiceCPUMillis,
		MemoryMebibytes: defaultServiceMemoryMebibytes,
	}
}

func TestCanonicalRollingStrategyAppliesTimingDefaults(t *testing.T) {
	t.Parallel()

	got := canonicalRollingStrategy(&platformv1.RollingStrategy{})
	if got.GetHealthcheckTimeoutSeconds() != 300 || got.GetDrainingSeconds() != 0 {
		t.Fatalf("timeout defaults = %+v", got)
	}
}

func TestCanonicalRollingStrategyPreservesExplicitTiming(t *testing.T) {
	t.Parallel()

	got := canonicalRollingStrategy(&platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: proto.Int32(60),
		DrainingSeconds:           proto.Int32(5),
	})
	if got.GetHealthcheckTimeoutSeconds() != 60 || got.GetDrainingSeconds() != 5 {
		t.Fatalf("strategy = %+v, want healthcheck timeout 60s and draining time 5s", got)
	}
}

func TestValidateRollingStrategyRejectsInvalidHealthcheckTimeout(t *testing.T) {
	t.Parallel()

	spec := &platformv1.ServiceSpec{
		RollingStrategy: &platformv1.RollingStrategy{
			HealthcheckTimeoutSeconds: proto.Int32(0),
		},
	}
	if err := ValidateRollingStrategy(spec); err == nil || !strings.Contains(err.Error(), "healthcheck timeout") {
		t.Fatalf("got %v, want healthcheck timeout error", err)
	}
}

func TestInternalServiceHostnameUsesTheMemorableServiceName(t *testing.T) {
	t.Parallel()

	if got := InternalServiceHostname("Accurate Reflection", "service-1"); got != "accurate-reflection.mesh.internal" {
		t.Fatalf("unexpected internal hostname %q", got)
	}
	if got := internalServiceShortName("---", "9D6D-6A8E"); got != "service-9d6d6a8e" {
		t.Fatalf("unexpected fallback short name %q", got)
	}
}

func TestValidateVolumeReplicaCompatibility(t *testing.T) {
	t.Parallel()

	volumeSpec := &platformv1.ServiceSpec{
		Runtime: &platformv1.ServiceRuntime{VolumeName: "data"},
	}
	if err := validateVolumeReplicaCompatibility(volumeSpec, 1); err != nil {
		t.Fatalf("volume with 1 replica should be allowed: %v", err)
	}
	if err := validateVolumeReplicaCompatibility(nil, 3); err != nil {
		t.Fatalf("replicas without a volume should be allowed: %v", err)
	}
	err := validateVolumeReplicaCompatibility(volumeSpec, 2)
	if !errors.Is(err, ErrVolumeReplicaUnsupported) {
		t.Fatalf("expected errVolumeReplicaUnsupported, got %v", err)
	}
}
