package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	errVolumeInUse              = errors.New("volume still referenced by service")
	errVolumeNotFound           = errors.New("volume not found")
	errVolumeAgentMismatch      = errors.New("volume bound to different agent")
	errConcurrentUpdate         = errors.New("concurrent service update")
	errDomainAlreadyExists      = errors.New("domain binding already exists")
	errInvalidPort              = errors.New("port must be an integer between 1 and 65535")
	errNoPlacementAvailable     = errors.New("no healthy agent satisfies placement")
	errInvalidReplicaCount      = errors.New("desired replica count is invalid")
	errVolumeReplicaUnsupported = errors.New("volume-backed services support a single replica")
)

const (
	defaultDesiredReplicaCount = 1
	maxDesiredReplicaCount     = 64
	maxAllocationRestartEvents = 20
)

func volumeKey(environmentID, name string) string {
	var b strings.Builder
	b.Grow(len(environmentID) + 1 + len(name))
	b.WriteString(environmentID)
	b.WriteByte(0)
	b.WriteString(name)
	return b.String()
}

type serviceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type jsonInt32Slice []int32

func (p *jsonInt32Slice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]int32)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]int32)(p))
	default:
		return fmt.Errorf("scan int32 slice json: unsupported type %T", src)
	}
}

func encodeHealthyPorts(ports []int32) ([]byte, error) {
	if ports == nil {
		ports = []int32{}
	}
	return json.Marshal(ports)
}

func encodeRestartObservation(obs *platformv1.RestartObservation) ([]byte, error) {
	if obs == nil {
		return []byte("{}"), nil
	}
	raw, err := protojson.Marshal(obs)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func decodeRestartObservation(raw []byte) (*platformv1.RestartObservation, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}
	obs := &platformv1.RestartObservation{}
	if err := protojson.Unmarshal(raw, obs); err != nil {
		return nil, err
	}
	return obs, nil
}

func canonicalServiceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSpec {
	if spec == nil {
		return nil
	}
	out := proto.Clone(spec).(*platformv1.ServiceSpec)
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
	if source := out.GetSource(); source != nil {
		if spec := source.GetSourceSpec(); spec != nil {
			spec.Provider = strings.TrimSpace(strings.ToLower(spec.GetProvider()))
			spec.RepositorySelector = strings.TrimSpace(strings.ToLower(spec.GetRepositorySelector()))
			spec.TrackedRef = strings.TrimSpace(spec.GetTrackedRef())
			if spec.TrackedRef == "" {
				spec.TrackedRef = "main"
			}
			if spec.BuildRecipe == nil {
				spec.BuildRecipe = &platformv1.BuildRecipe{}
			}
			if spec.BuildRecipe.DockerfilePath == "" {
				spec.BuildRecipe.DockerfilePath = "Dockerfile"
			}
			if spec.BuildRecipe.ContextDir == "" {
				spec.BuildRecipe.ContextDir = "."
			}
		}
	}
	return out
}

func serviceRuntime(spec *platformv1.ServiceSpec) *platformv1.ServiceRuntime {
	if spec == nil {
		return nil
	}
	return spec.GetRuntime()
}

func serviceVolumeName(spec *platformv1.ServiceSpec) string {
	return serviceRuntime(spec).GetVolumeName()
}

func validateVolumeReplicaCompatibility(spec *platformv1.ServiceSpec, desiredReplicaCount int32) error {
	volumeName := strings.TrimSpace(serviceVolumeName(spec))
	if volumeName == "" || desiredReplicaCount <= 1 {
		return nil
	}
	return fmt.Errorf("%w: %q", errVolumeReplicaUnsupported, volumeName)
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

func desiredSourceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSpec {
	if spec == nil || spec.GetSource() == nil {
		return nil
	}
	return spec.GetSource().GetSourceSpec()
}

func sameDesiredSourceSpec(a, b *platformv1.ServiceSpec) bool {
	as := desiredSourceSpec(a)
	bs := desiredSourceSpec(b)
	if (as == nil) != (bs == nil) {
		return false
	}
	if as == nil {
		return true
	}
	if strings.TrimSpace(strings.ToLower(as.GetProvider())) != strings.TrimSpace(strings.ToLower(bs.GetProvider())) {
		return false
	}
	if strings.TrimSpace(strings.ToLower(as.GetRepositorySelector())) != strings.TrimSpace(strings.ToLower(bs.GetRepositorySelector())) {
		return false
	}
	at := as.GetTrackedRef()
	bt := bs.GetTrackedRef()
	if at != bt && !(at == "" && bt == "main") && !(at == "main" && bt == "") {
		return false
	}
	ab := as.GetBuildRecipe()
	bb := bs.GetBuildRecipe()
	if (ab == nil) != (bb == nil) {
		return false
	}
	if ab == nil {
		return true
	}
	adp := ab.GetDockerfilePath()
	bdp := bb.GetDockerfilePath()
	if adp != bdp && !(adp == "" && bdp == "Dockerfile") && !(adp == "Dockerfile" && bdp == "") {
		return false
	}
	acd := ab.GetContextDir()
	bcd := bb.GetContextDir()
	if acd != bcd && !(acd == "" && bcd == ".") && !(acd == "." && bcd == "") {
		return false
	}
	return true
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
		if validatePort(port) != nil {
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
		if validatePort(port) != nil {
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

func buildSourceSummary(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSummary {
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

type placementCandidate struct {
	ID                     string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func sortedDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	sort.Strings(out)
	return out
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
		ag := as.GetSourceSpec()
		bg := bs.GetSourceSpec()
		if (ag == nil) != (bg == nil) {
			return false
		}
		if ag != nil && bg != nil {
			if strings.TrimSpace(strings.ToLower(ag.GetProvider())) != strings.TrimSpace(strings.ToLower(bg.GetProvider())) {
				return false
			}
			if strings.TrimSpace(strings.ToLower(ag.GetRepositorySelector())) != strings.TrimSpace(strings.ToLower(bg.GetRepositorySelector())) {
				return false
			}
			ab := ag.GetTrackedRef()
			bb := bg.GetTrackedRef()
			if ab != bb && !(ab == "" && bb == "main") && !(ab == "main" && bb == "") {
				return false
			}
			adp := ag.GetBuildRecipe().GetDockerfilePath()
			bdp := bg.GetBuildRecipe().GetDockerfilePath()
			if adp != bdp && !(adp == "" && bdp == "Dockerfile") && !(adp == "Dockerfile" && bdp == "") {
				return false
			}
			acd := ag.GetBuildRecipe().GetContextDir()
			bcd := bg.GetBuildRecipe().GetContextDir()
			if acd != bcd && !(acd == "" && bcd == ".") && !(acd == "." && bcd == "") {
				return false
			}
		}
	}
	return true
}

func equalRuntimeAfterCanonicalization(a, b *platformv1.ServiceRuntime) bool {
	ac := a.GetCommand()
	bc := b.GetCommand()
	switch {
	case len(ac) == 0 && len(bc) == 0:
	case len(ac) == 0 && len(bc) > 0:
		return false
	case len(ac) > 0 && len(bc) == 0:
		return false
	case len(ac) > 0 && len(bc) > 0:
		if len(ac) != len(bc) {
			return false
		}
		for i := range ac {
			if ac[i] != bc[i] {
				return false
			}
		}
	default:
		return false
	}

	aa := a.GetArgs()
	ba := b.GetArgs()
	switch {
	case len(aa) == 0 && len(ba) == 0:
	case len(aa) == 0 && len(ba) > 0:
		return false
	case len(aa) > 0 && len(ba) == 0:
		return false
	case len(aa) > 0 && len(ba) > 0:
		if len(aa) != len(ba) {
			return false
		}
		for i := range aa {
			if aa[i] != ba[i] {
				return false
			}
		}
	default:
		return false
	}

	ae := a.GetEnv()
	be := b.GetEnv()
	switch {
	case len(ae) == 0 && len(be) == 0:
	case len(ae) == 0 && len(be) > 0:
		return false
	case len(ae) > 0 && len(be) == 0:
		return false
	case len(ae) > 0 && len(be) > 0:
		if len(ae) != len(be) {
			return false
		}
		for i := range ae {
			if ae[i] != be[i] {
				return false
			}
		}
	default:
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
	return proto.Equal(a.GetRestart(), b.GetRestart())
}

func loadServiceSpec(raw []byte) (*platformv1.ServiceSpec, error) {
	spec := &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(raw, spec); err != nil {
		return nil, err
	}
	return canonicalServiceSpec(spec), nil
}

func (s *Store) markAllocationHealthyForTest(ctx context.Context, serviceID, allocationIP string, healthyPorts ...int32) error {
	encodedPorts, err := encodeHealthyPorts(healthyPorts)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE allocations
			    SET healthy = TRUE,
			        allocation_ip = $1,
			        healthy_ports = $2,
			        applied_spec_revision = GREATEST(applied_spec_revision, desired_spec_revision),
			        applied_rollout_generation = GREATEST(applied_rollout_generation, desired_rollout_generation),
			        updated_at = $3
			  WHERE service_id = $4`,
			allocationIP, encodedPorts, time.Now().UTC(), serviceID,
		); err != nil {
			return err
		}
		current, ok, err := s.currentDeploymentTx(ctx, tx, serviceID)
		if err != nil || !ok {
			return err
		}
		_, err = s.applyDeploymentTransitionTx(ctx, tx, current.ID, deploymentTransitionInput{
			ToState:          deploymentStateActive,
			Actor:            deploymentActor{Kind: deploymentCauseSystem},
			ReasonCode:       reasonDeploymentActive,
			Detail:           "Marked healthy for test",
			IgnoreIfTerminal: false,
		})
		return err
	})
}

func (s *Store) markAllocationIDHealthyForTest(ctx context.Context, allocationID, allocationIP string, healthyPorts ...int32) error {
	encodedPorts, err := encodeHealthyPorts(healthyPorts)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE allocations
		    SET healthy = TRUE,
		        allocation_ip = $1,
		        healthy_ports = $2,
		        applied_spec_revision = GREATEST(applied_spec_revision, desired_spec_revision),
		        applied_rollout_generation = GREATEST(applied_rollout_generation, desired_rollout_generation),
		        updated_at = $3
		  WHERE id = $4`,
		allocationIP, encodedPorts, time.Now().UTC(), allocationID,
	)
	return err
}

func (s *Store) countServiceRevisionsForTest(ctx context.Context, serviceID string) (int, error) {
	var revisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revisions); err != nil {
		return 0, err
	}
	return revisions, nil
}

func (s *Store) countServiceRolloutsForTest(ctx context.Context, serviceID string) (int, error) {
	var rollouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_rollouts WHERE service_id = $1`, serviceID).Scan(&rollouts); err != nil {
		return 0, err
	}
	return rollouts, nil
}
