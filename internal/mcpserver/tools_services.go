package mcpserver

import (
	"context"
	"fmt"
	"sort"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
)

const (
	defaultCPUMillis      = 250
	defaultMemoryMebibyte = 256
)

type serviceIDIn struct {
	ServiceID string `json:"service_id" jsonschema:"service ID (defaults to linked service)"`
}

// servicePatch carries friendly service settings shared by create and
// update. Pointer/slice emptiness distinguishes "keep" from "set" on
// update; on create, empty fields take platform defaults.
type servicePatch struct {
	Image             string            `json:"image,omitempty" jsonschema:"direct container image (e.g. postgres:16); alternative to source"`
	Repository        string            `json:"repository,omitempty" jsonschema:"source repository selector to build from (alternative to image)"`
	TrackedRef        string            `json:"tracked_ref,omitempty" jsonschema:"branch/ref to track for source builds (default: repository default branch)"`
	Provider          string            `json:"provider,omitempty" jsonschema:"source provider (default github)"`
	Builder           string            `json:"builder,omitempty" jsonschema:"railpack (default) or dockerfile"`
	DockerfilePath    string            `json:"dockerfile_path,omitempty"`
	BuildContextDir   string            `json:"build_context_dir,omitempty"`
	Command           []string          `json:"command,omitempty" jsonschema:"entrypoint override"`
	Args              []string          `json:"args,omitempty" jsonschema:"command arguments"`
	Env               map[string]string `json:"env,omitempty" jsonschema:"environment variables; on update these merge into the existing set (see set_variables)"`
	CPUMillis         int64             `json:"cpu_millis,omitempty" jsonschema:"CPU in millicores (1000 = 1 vCPU; default 250)"`
	MemoryMebibytes   int64             `json:"memory_mebibytes,omitempty" jsonschema:"memory in MiB (default 256)"`
	Ports             []int32           `json:"ports,omitempty" jsonschema:"container ports the service listens on; first entry is primary when primary_port is unset"`
	PrimaryPort       int32             `json:"primary_port,omitempty"`
	Replicas          *int32            `json:"replicas,omitempty" jsonschema:"desired replica count"`
	HealthCheckPath   string            `json:"health_check_path,omitempty" jsonschema:"HTTP readiness path"`
	HealthCheckPort   int32             `json:"health_check_port,omitempty"`
	LivenessPath      string            `json:"liveness_path,omitempty" jsonschema:"HTTP liveness path"`
	RestartPolicy     string            `json:"restart_policy,omitempty" jsonschema:"always, on_failure, or never"`
	MaxRestarts       int32             `json:"max_restarts,omitempty"`
	VolumeName        string            `json:"volume_name,omitempty" jsonschema:"environment volume to mount (at most one)"`
	VolumeMountPath   string            `json:"volume_mount_path,omitempty" jsonschema:"absolute mount path inside the container"`
	DetachVolume      bool              `json:"detach_volume,omitempty" jsonschema:"update only: remove the volume mount"`
	PlacementRegion   string            `json:"placement_region,omitempty" jsonschema:"operator-defined region constraint"`
	ServerlessEnabled *bool             `json:"serverless_enabled,omitempty" jsonschema:"scale to zero after an idle period with no network activity; wakes on the next public or private request (first request pays a cold start)"`
	RollingHealthSecs *int32            `json:"rolling_healthcheck_timeout_seconds,omitempty"`
	RollingDrainSecs  *int32            `json:"rolling_draining_seconds,omitempty"`
}

func buildRecipe(p servicePatch) *platformv1.BuildRecipe {
	recipe := &platformv1.BuildRecipe{DockerfilePath: p.DockerfilePath, ContextDir: p.BuildContextDir}
	switch p.Builder {
	case "dockerfile":
		recipe.Builder = platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE
	case "railpack":
		recipe.Builder = platformv1.BuilderKind_BUILDER_KIND_RAILPACK
	case "":
		if p.DockerfilePath != "" {
			recipe.Builder = platformv1.BuilderKind_BUILDER_KIND_DOCKERFILE
		}
	}
	return recipe
}

func runtimePorts(p servicePatch) []*platformv1.ServiceRuntimePort {
	out := make([]*platformv1.ServiceRuntimePort, 0, len(p.Ports))
	for i, port := range p.Ports {
		primary := port == p.PrimaryPort || (p.PrimaryPort == 0 && i == 0)
		out = append(out, &platformv1.ServiceRuntimePort{Port: port, Primary: primary})
	}
	return out
}

func restartPolicy(raw string) (platformv1.RestartPolicy, error) {
	switch raw {
	case "", "always":
		return platformv1.RestartPolicy_RESTART_POLICY_ALWAYS, nil
	case "on_failure":
		return platformv1.RestartPolicy_RESTART_POLICY_ON_FAILURE, nil
	case "never":
		return platformv1.RestartPolicy_RESTART_POLICY_NEVER, nil
	default:
		if value, ok := platformv1.RestartPolicy_value[raw]; ok {
			return platformv1.RestartPolicy(value), nil
		}
		return 0, fmt.Errorf("restart_policy must be always, on_failure, or never")
	}
}

// specFromPatch builds a full ServiceSpec from scratch (create).
func specFromPatch(p servicePatch) (*platformv1.ServiceSpec, error) {
	policy, err := restartPolicy(p.RestartPolicy)
	if err != nil {
		return nil, err
	}
	cpu := p.CPUMillis
	if cpu == 0 {
		cpu = defaultCPUMillis
	}
	memory := p.MemoryMebibytes
	if memory == 0 {
		memory = defaultMemoryMebibyte
	}
	runtime := &platformv1.ServiceRuntime{
		Command: p.Command, Args: p.Args, Env: p.Env,
		CpuMillis: cpu, MemoryMebibytes: memory,
		Ports:   runtimePorts(p),
		Restart: &platformv1.ServiceRestart{Policy: policy, MaxRestarts: p.MaxRestarts},
	}
	if p.HealthCheckPath != "" {
		runtime.HealthCheck = &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: p.HealthCheckPath, Port: p.HealthCheckPort}
	}
	if p.LivenessPath != "" {
		runtime.LivenessCheck = &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: p.LivenessPath, Port: p.HealthCheckPort}
	}
	if p.VolumeName != "" {
		runtime.Volume = &platformv1.ServiceVolumeMount{VolumeName: p.VolumeName, MountPath: p.VolumeMountPath}
	}
	spec := &platformv1.ServiceSpec{
		Runtime: runtime, PlacementRegion: p.PlacementRegion,
		DesiredReplicaCount: p.Replicas,
		ServerlessEnabled:   p.ServerlessEnabled,
	}
	if p.RollingHealthSecs != nil || p.RollingDrainSecs != nil {
		spec.RollingStrategy = &platformv1.RollingStrategy{
			HealthcheckTimeoutSeconds: p.RollingHealthSecs, DrainingSeconds: p.RollingDrainSecs,
		}
	}
	switch {
	case p.Image != "" && p.Repository != "":
		return nil, fmt.Errorf("set either image or repository, not both")
	case p.Image != "":
		spec.Source = &platformv1.ServiceSource{Source: &platformv1.ServiceSource_Image{Image: &platformv1.DirectImageSource{Image: p.Image}}}
	case p.Repository != "":
		provider := p.Provider
		if provider == "" {
			provider = "github"
		}
		spec.Source = &platformv1.ServiceSource{Source: &platformv1.ServiceSource_SourceSpec{SourceSpec: &platformv1.ServiceSourceSpec{
			Provider: provider, RepositorySelector: p.Repository, TrackedRef: p.TrackedRef, BuildRecipe: buildRecipe(p),
		}}}
	default:
		return nil, fmt.Errorf("image or repository is required: services run a direct image or build from source")
	}
	return spec, nil
}

// applyPatch merges a patch into a cloned current spec (update).
func applyPatch(current *platformv1.ServiceSpec, p servicePatch) (*platformv1.ServiceSpec, error) {
	spec := proto.Clone(current).(*platformv1.ServiceSpec)
	if spec.Runtime == nil {
		spec.Runtime = &platformv1.ServiceRuntime{}
	}
	runtime := spec.Runtime
	if p.Command != nil {
		runtime.Command = p.Command
	}
	if p.Args != nil {
		runtime.Args = p.Args
	}
	if len(p.Env) > 0 {
		if runtime.Env == nil {
			runtime.Env = map[string]string{}
		}
		for key, value := range p.Env {
			runtime.Env[key] = value
		}
	}
	if p.CPUMillis > 0 {
		runtime.CpuMillis = p.CPUMillis
	}
	if p.MemoryMebibytes > 0 {
		runtime.MemoryMebibytes = p.MemoryMebibytes
	}
	if p.Ports != nil {
		runtime.Ports = runtimePorts(p)
	}
	if p.HealthCheckPath != "" {
		runtime.HealthCheck = &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: p.HealthCheckPath, Port: p.HealthCheckPort}
	}
	if p.LivenessPath != "" {
		runtime.LivenessCheck = &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: p.LivenessPath, Port: p.HealthCheckPort}
	}
	if p.RestartPolicy != "" || p.MaxRestarts > 0 {
		policy, err := restartPolicy(p.RestartPolicy)
		if err != nil {
			return nil, err
		}
		if runtime.Restart == nil {
			runtime.Restart = &platformv1.ServiceRestart{}
		}
		if p.RestartPolicy != "" {
			runtime.Restart.Policy = policy
		}
		if p.MaxRestarts > 0 {
			runtime.Restart.MaxRestarts = p.MaxRestarts
		}
	}
	if p.DetachVolume {
		runtime.Volume = nil
	} else if p.VolumeName != "" {
		runtime.Volume = &platformv1.ServiceVolumeMount{VolumeName: p.VolumeName, MountPath: p.VolumeMountPath}
	}
	if p.Replicas != nil {
		spec.DesiredReplicaCount = p.Replicas
	}
	if p.PlacementRegion != "" {
		spec.PlacementRegion = p.PlacementRegion
	}
	if p.ServerlessEnabled != nil {
		spec.ServerlessEnabled = p.ServerlessEnabled
	}
	if p.RollingHealthSecs != nil || p.RollingDrainSecs != nil {
		if spec.RollingStrategy == nil {
			spec.RollingStrategy = &platformv1.RollingStrategy{}
		}
		if p.RollingHealthSecs != nil {
			spec.RollingStrategy.HealthcheckTimeoutSeconds = p.RollingHealthSecs
		}
		if p.RollingDrainSecs != nil {
			spec.RollingStrategy.DrainingSeconds = p.RollingDrainSecs
		}
	}
	switch {
	case p.Image != "" && p.Repository != "":
		return nil, fmt.Errorf("set either image or repository, not both")
	case p.Image != "":
		spec.Source = &platformv1.ServiceSource{Source: &platformv1.ServiceSource_Image{Image: &platformv1.DirectImageSource{Image: p.Image}}}
	case p.Repository != "":
		provider := p.Provider
		if provider == "" {
			provider = "github"
		}
		spec.Source = &platformv1.ServiceSource{Source: &platformv1.ServiceSource_SourceSpec{SourceSpec: &platformv1.ServiceSourceSpec{
			Provider: provider, RepositorySelector: p.Repository, TrackedRef: p.TrackedRef, BuildRecipe: buildRecipe(p),
		}}}
	case p.Builder != "" || p.DockerfilePath != "" || p.BuildContextDir != "" || p.TrackedRef != "":
		sourceSpec := spec.GetSource().GetSourceSpec()
		if sourceSpec == nil {
			return nil, fmt.Errorf("service uses a direct image; set repository to switch to source builds")
		}
		if p.TrackedRef != "" {
			sourceSpec.TrackedRef = p.TrackedRef
		}
		sourceSpec.BuildRecipe = buildRecipe(p)
	}
	return spec, nil
}

func (s *state) maybeRelease(ctx context.Context, environmentID string, release bool) (any, error) {
	if !release {
		return nil, nil
	}
	resp, err := s.client.ReleaseEnvironment(ctx, environmentID)
	if err != nil {
		return nil, err
	}
	return protoJSONList(resp.GetServices()), nil
}

func (s *state) addServiceTools(server *mcp.Server) {
	type listServicesIn struct {
		EnvironmentID  string `json:"environment_id"`
		IncludeDeleted bool   `json:"include_deleted,omitempty"`
	}
	add(server, readTool("list_services", "List services (and volumes) in an environment, with staged-change indicators."), func(ctx context.Context, in listServicesIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.ListServices(ctx, id, in.IncludeDeleted)
	})

	add(server, readTool("get_service", "Get a service: desired spec/draft, source state, latest build and deployment, pending changes."), func(ctx context.Context, in serviceIDIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.GetService(ctx, id)
	})

	add(server, readTool("get_service_status", "Service with live allocation status (placement, health, restarts)."), func(ctx context.Context, in serviceIDIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.GetServiceStatus(ctx, id)
	})

	type createServiceIn struct {
		EnvironmentID string `json:"environment_id"`
		Name          string `json:"name" jsonschema:"service name; services are reachable privately at their internal hostname"`
		Release       bool   `json:"release,omitempty" jsonschema:"deploy immediately after creating (release the environment)"`
		servicePatch
	}
	add(server, writeTool("create_service", "Create a service from a direct image (image) or a source repository build (repository). Staged until the environment is released unless release:true."), func(ctx context.Context, in createServiceIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		spec, err := specFromPatch(in.servicePatch)
		if err != nil {
			return nil, err
		}
		created, err := s.client.CreateService(ctx, &platformv1.CreateServiceRequest{
			EnvironmentId: id,
			Service:       &platformv1.ServiceInput{Name: in.Name, Spec: spec},
		})
		if err != nil {
			return nil, err
		}
		released, err := s.maybeRelease(ctx, id, in.Release)
		if err != nil {
			return nil, err
		}
		return map[string]any{"service": protoJSON(created), "note": stagedNote(in.Release), "released_services": released}, nil
	})

	type updateServiceIn struct {
		ServiceID string `json:"service_id"`
		Name      string `json:"name,omitempty" jsonschema:"rename the service"`
		Release   bool   `json:"release,omitempty" jsonschema:"deploy immediately after updating"`
		servicePatch
	}
	add(server, writeTool("update_service", "Patch a service draft (image, source, resources, ports, health checks, restart, volume mount, replicas, env merge). Only provided fields change. Staged until release unless release:true."), func(ctx context.Context, in updateServiceIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		current, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		spec, err := applyPatch(current.GetSpec(), in.servicePatch)
		if err != nil {
			return nil, err
		}
		name := in.Name
		if name == "" {
			name = current.GetName()
		}
		updated, err := s.client.UpdateService(ctx, &platformv1.UpdateServiceRequest{
			ServiceId:            id,
			Service:              &platformv1.ServiceUpdate{Name: name, Spec: spec},
			ExpectedSpecRevision: current.GetSpecRevision(),
		})
		if err != nil {
			return nil, err
		}
		released, err := s.maybeRelease(ctx, current.GetEnvironmentId(), in.Release)
		if err != nil {
			return nil, err
		}
		return map[string]any{"service": protoJSON(updated), "note": stagedNote(in.Release), "released_services": released}, nil
	})

	type scaleIn struct {
		ServiceID string `json:"service_id"`
		Replicas  int32  `json:"replicas" jsonschema:"desired replica count"`
	}
	add(server, idempotentWriteTool("scale_service", "Scale a service to a replica count (applies immediately, not staged)."), func(ctx context.Context, in scaleIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.ScaleService(ctx, id, in.Replicas)
	})

	type removeServiceIn struct {
		ServiceID string `json:"service_id"`
		Confirm   bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("remove_service", "Delete a service (restorable during grace; domain bindings go with it). Destructive: preview first, then confirm:true."), func(ctx context.Context, in removeServiceIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		svc, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		if !in.Confirm {
			domains, _ := s.client.ListDomains(ctx, id, false)
			return preview(fmt.Sprintf("delete service %q (%s) in environment %s", svc.GetName(), id, svc.GetEnvironmentId()),
				map[string]any{"service": protoJSON(svc), "domain_bindings": protoJSON(domains)}), nil
		}
		if err := s.client.DeleteService(ctx, id); err != nil {
			return nil, err
		}
		return deleted("service", id, svc.GetName()), nil
	})

	add(server, writeTool("restore_service", "Restore a deleted service within its grace period."), func(ctx context.Context, in serviceIDIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.RestoreService(ctx, id)
	})

	type discardIn struct {
		ServiceID  string   `json:"service_id"`
		ChangeIDs  []string `json:"change_ids,omitempty" jsonschema:"specific staged change IDs from get_service unapplied_changes"`
		DiscardAll bool     `json:"discard_all,omitempty" jsonschema:"discard every staged change"`
		Confirm    bool     `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("discard_service_changes", "Discard staged (unreleased) service changes. Destructive for drafts: preview first, then confirm:true."), func(ctx context.Context, in discardIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		if !in.Confirm {
			svc, err := s.client.GetService(ctx, id)
			if err != nil {
				return nil, err
			}
			return preview(fmt.Sprintf("discard staged changes of service %q (%s)", svc.GetName(), id),
				map[string]any{"unapplied_changes": protoJSONList(svc.GetUnappliedChanges())}), nil
		}
		if !in.DiscardAll && len(in.ChangeIDs) == 0 {
			return nil, fmt.Errorf("pass change_ids or discard_all: true")
		}
		return s.client.DiscardServiceChanges(ctx, &platformv1.DiscardServiceChangesRequest{
			ServiceId: id, ChangeIds: in.ChangeIDs, DiscardAll: in.DiscardAll,
		})
	})

	s.addVariableTools(server)
}

func (s *state) addVariableTools(server *mcp.Server) {
	type listVarsIn struct {
		ServiceID     string `json:"service_id"`
		IncludeValues *bool  `json:"include_values,omitempty" jsonschema:"include values (default true; values may hold credentials)"`
	}
	add(server, readTool("list_variables", "List a service's environment variables from its current draft, with values by default (Railway: pull these into a .env file when asked)."), func(ctx context.Context, in listVarsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		svc, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		env := svc.GetSpec().GetRuntime().GetEnv()
		out := map[string]any{
			"service_id": id, "service_name": svc.GetName(),
			"pending_changes": svc.GetPendingChanges(),
		}
		if in.IncludeValues != nil && !*in.IncludeValues {
			names := make([]string, 0, len(env))
			for name := range env {
				names = append(names, name)
			}
			sort.Strings(names)
			out["variable_names"] = names
		} else {
			out["variables"] = env
		}
		return out, nil
	})

	type setVarsIn struct {
		ServiceID string            `json:"service_id"`
		Set       map[string]string `json:"set,omitempty" jsonschema:"variables to set or overwrite"`
		Unset     []string          `json:"unset,omitempty" jsonschema:"variable names to remove"`
		Release   bool              `json:"release,omitempty" jsonschema:"deploy immediately after staging the change"`
	}
	add(server, writeTool("set_variables", "Set/unset environment variables on a service draft. Staged until release unless release:true. To reference another service's value, read it with list_variables and set the concrete value (no server-side reference syntax)."), func(ctx context.Context, in setVarsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		if len(in.Set) == 0 && len(in.Unset) == 0 {
			return nil, fmt.Errorf("pass set and/or unset")
		}
		current, err := s.client.GetService(ctx, id)
		if err != nil {
			return nil, err
		}
		spec := proto.Clone(current.GetSpec()).(*platformv1.ServiceSpec)
		if spec.Runtime == nil {
			spec.Runtime = &platformv1.ServiceRuntime{}
		}
		if spec.Runtime.Env == nil {
			spec.Runtime.Env = map[string]string{}
		}
		for key, value := range in.Set {
			spec.Runtime.Env[key] = value
		}
		for _, key := range in.Unset {
			delete(spec.Runtime.Env, key)
		}
		updated, err := s.client.UpdateService(ctx, &platformv1.UpdateServiceRequest{
			ServiceId:            id,
			Service:              &platformv1.ServiceUpdate{Name: current.GetName(), Spec: spec},
			ExpectedSpecRevision: current.GetSpecRevision(),
		})
		if err != nil {
			return nil, err
		}
		released, err := s.maybeRelease(ctx, current.GetEnvironmentId(), in.Release)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(spec.Runtime.Env))
		for name := range spec.Runtime.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		return map[string]any{
			"service": protoJSON(updated), "variable_names": names,
			"note": stagedNote(in.Release), "released_services": released,
		}, nil
	})
}
