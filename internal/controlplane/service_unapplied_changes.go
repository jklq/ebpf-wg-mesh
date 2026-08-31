package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/restartpolicy"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type unappliedChangeField struct {
	id      string
	section string
	field   string
	path    string
	current string
	next    string
}

func diffServiceUnappliedChanges(current, deployed *platformv1.ServiceSpec) []*platformv1.ServiceUnappliedChange {
	current = canonicalServiceSpec(current)
	deployed = canonicalServiceSpec(deployed)
	fields := serviceUnappliedChangeFields(current, deployed)
	out := make([]*platformv1.ServiceUnappliedChange, 0, len(fields))
	for _, field := range fields {
		if field.current == field.next {
			continue
		}
		action := platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_UPDATE
		switch {
		case field.current == "":
			action = platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_ADD
		case field.next == "":
			action = platformv1.ServiceUnappliedChangeAction_SERVICE_UNAPPLIED_CHANGE_ACTION_REMOVE
		}
		out = append(out, &platformv1.ServiceUnappliedChange{
			Id:           field.id,
			Section:      field.section,
			Field:        field.field,
			Path:         field.path,
			Action:       action,
			CurrentValue: field.current,
			NewValue:     field.next,
		})
	}
	return out
}

func serviceUnappliedChangeFields(current, deployed *platformv1.ServiceSpec) []unappliedChangeField {
	currentSource := desiredSourceSpec(current)
	deployedSource := desiredSourceSpec(deployed)
	currentRecipe := currentSource.GetBuildRecipe()
	deployedRecipe := deployedSource.GetBuildRecipe()
	fields := []unappliedChangeField{
		{
			id:      "source.repositorySelector",
			section: "Source",
			field:   "Repository",
			path:    "source.repositorySelector",
			current: deployedSource.GetRepositorySelector(),
			next:    currentSource.GetRepositorySelector(),
		},
		{
			id:      "source.trackedRef",
			section: "Source",
			field:   "Tracked ref",
			path:    "source.trackedRef",
			current: deployedSource.GetTrackedRef(),
			next:    currentSource.GetTrackedRef(),
		},
		{
			id:      "source.buildRecipe.dockerfilePath",
			section: "Build",
			field:   "Dockerfile path",
			path:    "source.buildRecipe.dockerfilePath",
			current: deployedRecipe.GetDockerfilePath(),
			next:    currentRecipe.GetDockerfilePath(),
		},
		{
			id:      "source.buildRecipe.contextDir",
			section: "Build",
			field:   "Context directory",
			path:    "source.buildRecipe.contextDir",
			current: deployedRecipe.GetContextDir(),
			next:    currentRecipe.GetContextDir(),
		},
		{
			id:      "source.image.ref",
			section: "Image",
			field:   "Image",
			path:    "source.image.ref",
			current: directImageRef(deployed),
			next:    directImageRef(current),
		},
	}

	for _, key := range sortedStringUnion(serviceRuntime(deployed).GetEnv(), serviceRuntime(current).GetEnv()) {
		fields = append(fields, unappliedChangeField{
			id:      "runtime.env." + key,
			section: "Variables",
			field:   key,
			path:    "runtime.env." + key,
			current: serviceRuntime(deployed).GetEnv()[key],
			next:    serviceRuntime(current).GetEnv()[key],
		})
	}
	for _, port := range sortedPortUnion(serviceRuntime(deployed).GetPorts(), serviceRuntime(current).GetPorts()) {
		key := strconv.Itoa(int(port))
		fields = append(fields, unappliedChangeField{
			id:      "runtime.ports." + key,
			section: "Ports",
			field:   key,
			path:    "runtime.ports." + key,
			current: portPresenceValue(deployed, port),
			next:    portPresenceValue(current, port),
		})
	}
	fields = append(fields, unappliedChangeField{
		id:      "runtime.healthCheck",
		section: "Health check",
		field:   "HTTP readiness check",
		path:    "runtime.healthCheck",
		current: healthCheckValue(serviceRuntime(deployed).GetHealthCheck()),
		next:    healthCheckValue(serviceRuntime(current).GetHealthCheck()),
	})
	fields = append(fields, unappliedChangeField{
		id:      "runtime.restart",
		section: "Restart",
		field:   "Process restart",
		path:    "runtime.restart",
		current: restartValue(serviceRuntime(deployed).GetRestart()),
		next:    restartValue(serviceRuntime(current).GetRestart()),
	})
	fields = append(fields, unappliedChangeField{
		id:      "runtime.sandboxProfile",
		section: "Workload isolation",
		field:   "Sandbox profile",
		path:    "runtime.sandboxProfile",
		current: sandboxProfileValue(serviceRuntime(deployed).GetSandboxProfile()),
		next:    sandboxProfileValue(serviceRuntime(current).GetSandboxProfile()),
	})
	fields = append(fields, unappliedChangeField{
		id:      "runtime.livenessCheck",
		section: "Health check",
		field:   "HTTP liveness check",
		path:    "runtime.livenessCheck",
		current: healthCheckValue(serviceRuntime(deployed).GetLivenessCheck()),
		next:    healthCheckValue(serviceRuntime(current).GetLivenessCheck()),
	})
	fields = append(fields, unappliedChangeField{
		id:      "desiredReplicaCount",
		section: "Replicas",
		field:   "Desired count",
		path:    "desiredReplicaCount",
		current: replicaCountValue(deployed),
		next:    replicaCountValue(current),
	})
	return fields
}

func replicaCountValue(spec *platformv1.ServiceSpec) string {
	return strconv.Itoa(int(specReplicaCount(spec, defaultDesiredReplicaCount)))
}

func restartValue(restart *platformv1.ServiceRestart) string {
	if restart == nil {
		return ""
	}
	return restartpolicy.FormatRestart(restart)
}

func sandboxProfileValue(profile *platformv1.SandboxProfile) string {
	if profile == nil {
		return productionSandboxProfileName
	}
	parts := []string{profile.GetName()}
	for _, relaxation := range profile.GetRelaxations() {
		parts = append(parts, relaxation.String())
	}
	parts = append(parts, profile.GetRisk())
	return strings.Join(parts, " | ")
}

func healthCheckValue(check *platformv1.HealthCheck) string {
	if check == nil || check.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED {
		return ""
	}
	port := "primary port"
	if check.GetPort() > 0 {
		port = "port " + strconv.Itoa(int(check.GetPort()))
	}
	timeout := ""
	if check.GetTimeoutSeconds() > 0 {
		timeout = ", timeout " + strconv.Itoa(int(check.GetTimeoutSeconds())) + "s"
	}
	return "GET " + check.GetPath() + " on " + port + timeout
}

func sortedStringUnion(a, b map[string]string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		seen[key] = struct{}{}
	}
	for key := range b {
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPortUnion(a, b []*platformv1.ServiceRuntimePort) []int32 {
	seen := make(map[int32]struct{}, len(a)+len(b))
	for _, port := range a {
		if validatePort(port.GetPort()) == nil {
			seen[port.GetPort()] = struct{}{}
		}
	}
	for _, port := range b {
		if validatePort(port.GetPort()) == nil {
			seen[port.GetPort()] = struct{}{}
		}
	}
	ports := make([]int32, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}

func portPresenceValue(spec *platformv1.ServiceSpec, port int32) string {
	for _, item := range serviceRuntime(spec).GetPorts() {
		if item.GetPort() == port {
			if item.GetPrimary() {
				return "primary"
			}
			return "enabled"
		}
	}
	return ""
}

func applyDiscardedServiceChanges(current, deployed *platformv1.ServiceSpec, discardAll bool, changeIDs []string) *platformv1.ServiceSpec {
	if discardAll {
		return canonicalServiceSpec(deployed)
	}
	out := canonicalServiceSpec(current)
	if out == nil {
		out = &platformv1.ServiceSpec{}
	}
	idSet := make(map[string]struct{}, len(changeIDs))
	for _, id := range changeIDs {
		idSet[id] = struct{}{}
	}
	for id := range idSet {
		applyDiscardedServiceChange(out, deployed, id)
	}
	return canonicalServiceSpec(out)
}

func applyDiscardedServiceChange(current, deployed *platformv1.ServiceSpec, id string) {
	switch id {
	case "source.repositorySelector":
		currentSourceSpec(current).RepositorySelector = desiredSourceSpec(deployed).GetRepositorySelector()
	case "source.trackedRef":
		currentSourceSpec(current).TrackedRef = desiredSourceSpec(deployed).GetTrackedRef()
	case "source.buildRecipe.dockerfilePath":
		currentSourceSpec(current).BuildRecipe.DockerfilePath = desiredSourceSpec(deployed).GetBuildRecipe().GetDockerfilePath()
	case "source.buildRecipe.contextDir":
		currentSourceSpec(current).BuildRecipe.ContextDir = desiredSourceSpec(deployed).GetBuildRecipe().GetContextDir()
	case "source.image.ref":
		current.Source = &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_Image{
				Image: &platformv1.DirectImageSource{Image: directImageRef(deployed)},
			},
		}
	case "runtime.healthCheck":
		deployedCheck := serviceRuntime(deployed).GetHealthCheck()
		if deployedCheck == nil {
			currentRuntime(current).HealthCheck = nil
		} else {
			currentRuntime(current).HealthCheck = proto.Clone(deployedCheck).(*platformv1.HealthCheck)
		}
	case "runtime.livenessCheck":
		deployedCheck := serviceRuntime(deployed).GetLivenessCheck()
		if deployedCheck == nil {
			currentRuntime(current).LivenessCheck = nil
		} else {
			currentRuntime(current).LivenessCheck = proto.Clone(deployedCheck).(*platformv1.HealthCheck)
		}
	case "runtime.restart":
		deployedRestart := serviceRuntime(deployed).GetRestart()
		if deployedRestart == nil {
			currentRuntime(current).Restart = nil
		} else {
			currentRuntime(current).Restart = proto.Clone(deployedRestart).(*platformv1.ServiceRestart)
		}
	case "runtime.sandboxProfile":
		deployedProfile := serviceRuntime(deployed).GetSandboxProfile()
		if deployedProfile == nil {
			currentRuntime(current).SandboxProfile = productionSandboxProfile()
		} else {
			currentRuntime(current).SandboxProfile = proto.Clone(deployedProfile).(*platformv1.SandboxProfile)
		}
	case "desiredReplicaCount":
		if specHasDesiredReplicaCount(deployed) {
			current.DesiredReplicaCount = replicaCountPtr(deployed.GetDesiredReplicaCount())
		} else {
			current.DesiredReplicaCount = nil
		}
	default:
		const envPrefix = "runtime.env."
		const portPrefix = "runtime.ports."
		switch {
		case len(id) > len(envPrefix) && id[:len(envPrefix)] == envPrefix:
			key := id[len(envPrefix):]
			runtime := currentRuntime(current)
			if deployedValue, ok := serviceRuntime(deployed).GetEnv()[key]; ok {
				if runtime.Env == nil {
					runtime.Env = map[string]string{}
				}
				runtime.Env[key] = deployedValue
			} else {
				delete(runtime.Env, key)
				if len(runtime.Env) == 0 {
					runtime.Env = nil
				}
			}
		case len(id) > len(portPrefix) && id[:len(portPrefix)] == portPrefix:
			port, err := strconv.ParseInt(id[len(portPrefix):], 10, 32)
			if err != nil {
				return
			}
			runtime := currentRuntime(current)
			runtime.Ports = discardRuntimePort(runtime.GetPorts(), serviceRuntime(deployed).GetPorts(), int32(port))
		}
	}
}

func currentRuntime(spec *platformv1.ServiceSpec) *platformv1.ServiceRuntime {
	if spec.Runtime == nil {
		spec.Runtime = &platformv1.ServiceRuntime{}
	}
	return spec.Runtime
}

func currentSourceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSpec {
	if spec.Source == nil || spec.Source.GetSourceSpec() == nil {
		spec.Source = &platformv1.ServiceSource{
			Source: &platformv1.ServiceSource_SourceSpec{
				SourceSpec: &platformv1.ServiceSourceSpec{},
			},
		}
	}
	source := spec.Source.GetSourceSpec()
	if source.BuildRecipe == nil {
		source.BuildRecipe = &platformv1.BuildRecipe{}
	}
	return source
}

func discardRuntimePort(current, deployed []*platformv1.ServiceRuntimePort, port int32) []*platformv1.ServiceRuntimePort {
	out := make([]*platformv1.ServiceRuntimePort, 0, len(current)+1)
	for _, item := range current {
		if item.GetPort() != port {
			out = append(out, proto.Clone(item).(*platformv1.ServiceRuntimePort))
		}
	}
	for _, item := range deployed {
		if item.GetPort() == port {
			out = append(out, proto.Clone(item).(*platformv1.ServiceRuntimePort))
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetPort() < out[j].GetPort() })
	return out
}

func (s *Store) loadServiceUnappliedChangesQuerier(ctx context.Context, q serviceQueryer, serviceID string, current *platformv1.ServiceSpec, rolloutGeneration int64) ([]*platformv1.ServiceUnappliedChange, *platformv1.ServiceSpec, error) {
	deployed, err := s.loadDeployedServiceSpecQuerier(ctx, q, serviceID, rolloutGeneration)
	if err != nil {
		return nil, nil, err
	}
	return diffServiceUnappliedChanges(current, deployed), deployed, nil
}

func (s *Store) loadDeployedServiceSpecQuerier(ctx context.Context, q serviceQueryer, serviceID string, rolloutGeneration int64) (*platformv1.ServiceSpec, error) {
	if rolloutGeneration == 0 {
		return canonicalServiceSpec(nil), nil
	}
	var rawSpec []byte
	if err := q.QueryRowContext(
		ctx,
		`SELECT r.spec_json
		   FROM service_rollouts ro
		   JOIN service_revisions r
		     ON r.service_id = ro.service_id
		    AND r.spec_revision = ro.spec_revision
		  WHERE ro.service_id = $1 AND ro.rollout_generation = $2`,
		serviceID,
		rolloutGeneration,
	).Scan(&rawSpec); err != nil {
		return nil, err
	}
	spec, err := loadServiceSpec(rawSpec)
	if err != nil {
		return nil, err
	}
	return canonicalServiceSpec(spec), nil
}

func (s *Store) discardServiceChanges(ctx context.Context, userID, projectID, serviceID string, changeIDs []string, discardAll bool) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := s.serviceByIDQuerier(ctx, tx, userID, projectID, serviceID)
		if err != nil {
			return err
		}
		changes, deployed, err := s.loadServiceUnappliedChangesQuerier(ctx, tx, current.ID, current.Spec, current.RolloutGeneration)
		if err != nil {
			return err
		}
		valid := make(map[string]struct{}, len(changes))
		for _, change := range changes {
			valid[change.GetId()] = struct{}{}
		}
		filtered := make([]string, 0, len(changeIDs))
		for _, id := range changeIDs {
			if _, ok := valid[id]; ok {
				filtered = append(filtered, id)
			}
		}
		if !discardAll && len(filtered) == 0 {
			rec = current
			return nil
		}
		nextSpec := applyDiscardedServiceChanges(current.Spec, deployed, discardAll, filtered)
		if sameServiceSpec(current.Spec, nextSpec) {
			rec = current
			return nil
		}
		now := time.Now().UTC()
		nextRevision := current.SpecRevision + 1
		specJSON, err := protojson.Marshal(nextSpec)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE services
			    SET current_spec_revision = $1,
			        updated_at = $2
			  WHERE id = $3
			    AND current_spec_revision = $4
			    AND current_rollout_generation = $5`,
			nextRevision, now, current.ID, current.SpecRevision, current.RolloutGeneration,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return errConcurrentUpdate
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			current.ID, nextRevision, specJSON, now,
		); err != nil {
			return err
		}
		rec = current
		rec.Spec = nextSpec
		rec.SpecRevision = nextRevision
		rec.UpdatedAt = now
		if source := desiredSourceSpec(nextSpec); source != nil {
			rec.SourceSummary = toProtoSourceStateSummary(source, nil, nil, nil)
		} else {
			rec.SourceSummary = buildSourceSummary(nextSpec)
		}
		rec.ResolvedImage = directImageRef(nextSpec)
		return nil
	})
	if err != nil {
		return serviceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	rec, err = s.serviceByID(ctx, userID, projectID, serviceID)
	if err != nil {
		return serviceRecord{}, fmt.Errorf("discard service changes: %w", err)
	}
	return rec, nil
}
