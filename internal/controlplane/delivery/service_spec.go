package delivery

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultDesiredReplicaCount = 1
	MaxDesiredReplicaCount     = 64
)

func CanonicalServiceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSpec {
	if spec == nil {
		return nil
	}
	out := proto.Clone(spec).(*platformv1.ServiceSpec)
	out.PlacementRegion = strings.ToLower(strings.TrimSpace(out.GetPlacementRegion()))
	out.RollingStrategy = canonicalRollingStrategy(out.GetRollingStrategy())
	runtime := out.GetRuntime()
	if runtime != nil {
		if len(runtime.Command) == 0 {
			runtime.Command = nil
		}
		if len(runtime.Args) == 0 {
			runtime.Args = nil
		}
		if len(runtime.Env) == 0 {
			runtime.Env = nil
		}
		if hc := runtime.GetHealthCheck(); hc != nil &&
			hc.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED &&
			hc.GetPath() == "" &&
			hc.GetPort() == 0 &&
			hc.GetTimeoutSeconds() == 0 {
			runtime.HealthCheck = nil
		}
		if lc := runtime.GetLivenessCheck(); lc != nil &&
			lc.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED &&
			lc.GetPath() == "" &&
			lc.GetPort() == 0 &&
			lc.GetTimeoutSeconds() == 0 {
			runtime.LivenessCheck = nil
		}
		if err := restartpolicy.ValidateRestart(runtime.GetRestart()); err == nil {
			runtime.Restart = restartpolicy.CanonicalRestart(runtime.GetRestart())
		}
	}
	if desired := out.GetSource(); desired != nil {
		if spec := desired.GetSourceSpec(); spec != nil {
			spec.Provider = strings.TrimSpace(strings.ToLower(spec.GetProvider()))
			spec.RepositorySelector = strings.TrimSpace(strings.ToLower(spec.GetRepositorySelector()))
			spec.TrackedRef = strings.TrimSpace(spec.GetTrackedRef())
			if spec.TrackedRef == "" {
				spec.TrackedRef = "main"
			}
			if spec.BuildRecipe == nil {
				spec.BuildRecipe = &platformv1.BuildRecipe{}
			}
			spec.BuildRecipe.DockerfilePath = strings.TrimSpace(spec.BuildRecipe.GetDockerfilePath())
			spec.BuildRecipe.ContextDir = strings.TrimSpace(spec.BuildRecipe.GetContextDir())
			if spec.BuildRecipe.Builder == platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE && spec.BuildRecipe.DockerfilePath == "" {
				spec.BuildRecipe.DockerfilePath = "Dockerfile"
			}
			if spec.BuildRecipe.ContextDir == "" {
				spec.BuildRecipe.ContextDir = "."
			}
		}
	}
	return out
}

func ValidateServicePlacement(spec *platformv1.ServiceSpec) error {
	region := strings.TrimSpace(spec.GetPlacementRegion())
	if region == "" {
		return nil
	}
	if !fleetLabelPattern.MatchString(region) {
		return errors.New("placement region must be a lowercase operator region label")
	}
	return nil
}

func ValidateBuildRecipe(spec *platformv1.ServiceSpec) error {
	if source.DesiredSourceSpec(spec) == nil {
		return nil
	}
	switch source.DesiredSourceSpec(spec).GetBuildRecipe().GetBuilder() {
	case platformv1.BuilderKind_BUILDER_KIND_RAILPACK, platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE:
		return nil
	default:
		return errors.New("build recipe builder is required: railpack or dockerfile")
	}
}

func serviceRuntime(spec *platformv1.ServiceSpec) *platformv1.ServiceRuntime {
	if spec == nil {
		return nil
	}
	return spec.GetRuntime()
}

func ServiceVolumeName(spec *platformv1.ServiceSpec) string {
	return serviceRuntime(spec).GetVolumeName()
}

func validateVolumeReplicaCompatibility(spec *platformv1.ServiceSpec, desiredReplicaCount int32) error {
	volumeName := strings.TrimSpace(ServiceVolumeName(spec))
	if volumeName == "" || desiredReplicaCount <= 1 {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrVolumeReplicaUnsupported, volumeName)
}

func specHasDesiredReplicaCount(spec *platformv1.ServiceSpec) bool {
	return spec != nil && spec.DesiredReplicaCount != nil
}

func specReplicaCount(spec *platformv1.ServiceSpec, fallback int32) int32 {
	if specHasDesiredReplicaCount(spec) {
		return spec.GetDesiredReplicaCount()
	}
	return fallback
}

func replicaCountPtr(count int32) *int32 {
	value := count
	return &value
}

func directImageRef(spec *platformv1.ServiceSpec) string {
	if spec == nil || spec.GetSource() == nil {
		return ""
	}
	return spec.GetSource().GetImage().GetImage()
}

func sameDesiredSourceSpec(a, b *platformv1.ServiceSpec) bool {
	return equalDesiredSourceSpec(source.DesiredSourceSpec(a), source.DesiredSourceSpec(b))
}

func equalDesiredSourceSpec(a, b *platformv1.ServiceSourceSpec) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if normLower(a.GetProvider()) != normLower(b.GetProvider()) {
		return false
	}
	if normLower(a.GetRepositorySelector()) != normLower(b.GetRepositorySelector()) {
		return false
	}
	if !sameWithDefault(a.GetTrackedRef(), b.GetTrackedRef(), "main") {
		return false
	}
	ar, br := a.GetBuildRecipe(), b.GetBuildRecipe()
	if (ar == nil) != (br == nil) {
		return false
	}
	if ar == nil {
		return true
	}
	if ar.GetBuilder() != br.GetBuilder() {
		return false
	}
	return sameWithDefault(ar.GetDockerfilePath(), br.GetDockerfilePath(), "Dockerfile") &&
		sameWithDefault(ar.GetContextDir(), br.GetContextDir(), ".")
}

func normLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func sameWithDefault(a, b, def string) bool {
	return a == b || (a == "" && b == def) || (a == def && b == "")
}

func resolvedServiceSpec(spec *platformv1.ServiceSpec, resolvedImage string) *platformv1.ResolvedServiceSpec {
	if spec == nil {
		return nil
	}
	image := resolvedImage
	if image == "" {
		image = directImageRef(spec)
	}
	var runtime *platformv1.ServiceRuntime
	if spec.GetRuntime() != nil {
		runtime = proto.Clone(spec.GetRuntime()).(*platformv1.ServiceRuntime)
	}
	return &platformv1.ResolvedServiceSpec{
		Image:   image,
		Runtime: runtime,
	}
}

func resolvedDesiredServiceSpec(spec *platformv1.ServiceSpec, resolvedImage string, domainTargetPorts []int32) *platformv1.ResolvedServiceSpec {
	resolved := resolvedServiceSpec(spec, resolvedImage)
	if resolved == nil {
		return nil
	}
	if resolved.Runtime == nil {
		resolved.Runtime = &platformv1.ServiceRuntime{}
	}
	resolved.Runtime.Ports = unionRuntimePorts(resolved.Runtime.GetPorts(), domainTargetPorts)
	return resolved
}

func unionRuntimePorts(runtimePorts []*platformv1.ServiceRuntimePort, extraPorts []int32) []*platformv1.ServiceRuntimePort {
	seen := make(map[int32]struct{}, len(runtimePorts)+len(extraPorts))
	out := make([]*platformv1.ServiceRuntimePort, 0, len(runtimePorts)+len(extraPorts))
	hasPrimary := false
	for _, item := range runtimePorts {
		port := item.GetPort()
		if ValidatePort(port) != nil {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		next := proto.Clone(item).(*platformv1.ServiceRuntimePort)
		if next.GetPrimary() {
			if hasPrimary {
				next.Primary = false
			} else {
				hasPrimary = true
			}
		}
		out = append(out, next)
	}
	for _, port := range extraPorts {
		if ValidatePort(port) != nil {
			continue
		}
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		next := &platformv1.ServiceRuntimePort{Port: port}
		if !hasPrimary {
			next.Primary = true
			hasPrimary = true
		}
		out = append(out, next)
	}
	return out
}

func BuildSourceSummary(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSummary {
	if spec == nil || spec.GetSource() == nil {
		return nil
	}
	switch src := spec.GetSource().Source.(type) {
	case *platformv1.ServiceSource_Image:
		return &platformv1.ServiceSourceSummary{
			Source: &platformv1.ServiceSourceSummary_Image{
				Image: proto.Clone(src.Image).(*platformv1.DirectImageSource),
			},
		}
	}
	return nil
}

func sameServiceSpec(a, b *platformv1.ServiceSpec) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if !proto.Equal(a, b) {
		return false
	}
	return equalServiceSpecAfterCanonicalization(a, b)
}

func equalServiceSpecAfterCanonicalization(a, b *platformv1.ServiceSpec) bool {
	if !proto.Equal(canonicalRollingStrategy(a.GetRollingStrategy()), canonicalRollingStrategy(b.GetRollingStrategy())) {
		return false
	}
	ar := a.GetRuntime()
	br := b.GetRuntime()
	if (ar == nil) != (br == nil) {
		return false
	}
	if ar != nil {
		if !equalRuntimeAfterCanonicalization(ar, br) {
			return false
		}
	}
	as := a.GetSource()
	bs := b.GetSource()
	if (as == nil) != (bs == nil) {
		return false
	}
	if as != nil && bs != nil {
		if !equalDesiredSourceSpec(as.GetSourceSpec(), bs.GetSourceSpec()) {
			return false
		}
	}
	return true
}

func equalRuntimeAfterCanonicalization(a, b *platformv1.ServiceRuntime) bool {
	if !slices.Equal(a.GetCommand(), b.GetCommand()) {
		return false
	}
	if !slices.Equal(a.GetArgs(), b.GetArgs()) {
		return false
	}
	if !maps.Equal(a.GetEnv(), b.GetEnv()) {
		return false
	}

	ahc := a.GetHealthCheck()
	bhc := b.GetHealthCheck()
	if !proto.Equal(ahc, bhc) {
		return false
	}
	if !proto.Equal(a.GetLivenessCheck(), b.GetLivenessCheck()) {
		return false
	}
	if !proto.Equal(a.GetRestart(), b.GetRestart()) {
		return false
	}
	return true
}

func LoadServiceSpec(raw []byte) (*platformv1.ServiceSpec, error) {
	spec := &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(raw, spec); err != nil {
		return nil, err
	}
	return CanonicalServiceSpec(spec), nil
}

func ValidatePort(port int32) error {
	if port < 1 || port > 65535 {
		return ErrInvalidPort
	}
	return nil
}

const (
	defaultRolloutMaxUnavailable = 0
	defaultRolloutMaxSurge       = 1
	defaultRolloutSchedulingWait = 5 * time.Minute
	defaultHealthcheckTimeout    = 5 * time.Minute
	defaultDrainingTime          = time.Duration(0)
	maxRolloutDeadline           = 24 * time.Hour
)

var ErrVolumeRollingUnsupported = errors.New("volume-backed services cannot overlap rollout generations until volume handoff is supported")

var ErrRolloutInProgress = errors.New("a rollout is already in progress")

func canonicalRollingStrategy(strategy *platformv1.RollingStrategy) *platformv1.RollingStrategy {
	out := &platformv1.RollingStrategy{
		HealthcheckTimeoutSeconds: proto.Int32(int32(defaultHealthcheckTimeout / time.Second)),
		DrainingSeconds:           proto.Int32(int32(defaultDrainingTime / time.Second)),
	}
	if strategy == nil {
		return out
	}
	if strategy.HealthcheckTimeoutSeconds != nil {
		out.HealthcheckTimeoutSeconds = proto.Int32(strategy.GetHealthcheckTimeoutSeconds())
	}
	if strategy.DrainingSeconds != nil {
		out.DrainingSeconds = proto.Int32(strategy.GetDrainingSeconds())
	}
	return out
}

func ValidateRollingStrategy(spec *platformv1.ServiceSpec) error {
	strategy := canonicalRollingStrategy(spec.GetRollingStrategy())
	healthcheckTimeout := time.Duration(strategy.GetHealthcheckTimeoutSeconds()) * time.Second
	if healthcheckTimeout < time.Second || healthcheckTimeout > maxRolloutDeadline {
		return fmt.Errorf("healthcheck timeout must be between 1 second and %s", maxRolloutDeadline)
	}
	drainingTime := time.Duration(strategy.GetDrainingSeconds()) * time.Second
	if drainingTime < 0 || drainingTime > maxRolloutDeadline {
		return fmt.Errorf("draining time must be between 0 seconds and %s", maxRolloutDeadline)
	}
	return nil
}

const internalDomainSuffix = "mesh.internal"

func InternalServiceHostname(name, serviceID string) string {
	return internalServiceShortName(name, serviceID) + "." + internalDomainSuffix
}

func internalServiceShortName(name, serviceID string) string {
	var label strings.Builder
	label.Grow(len(name))
	separator := false
	for _, char := range strings.ToLower(strings.TrimSpace(name)) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			if separator && label.Len() > 0 {
				label.WriteByte('-')
			}
			separator = false
			label.WriteRune(char)
			continue
		}
		separator = true
	}
	shortName := strings.Trim(label.String(), "-")
	if shortName == "" {
		shortName = "service-" + compactServiceID(serviceID)
	}
	if len(shortName) > 63 {
		shortName = strings.TrimRight(shortName[:63], "-")
	}
	return shortName
}

func compactServiceID(serviceID string) string {
	var compact strings.Builder
	for _, char := range strings.ToLower(serviceID) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			compact.WriteRune(char)
			if compact.Len() == 12 {
				break
			}
		}
	}
	if compact.Len() == 0 {
		return "unknown"
	}
	return compact.String()
}

func volumeKey(environmentID, name string) string {
	var b strings.Builder
	b.Grow(len(environmentID) + 1 + len(name))
	b.WriteString(environmentID)
	b.WriteByte(0)
	b.WriteString(name)
	return b.String()
}
