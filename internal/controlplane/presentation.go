package controlplane

import (
	"slices"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func toProtoProject(rec deliverycore.ProjectRecord) *platformv1.Project {
	return &platformv1.Project{
		Id:               rec.ID,
		Name:             rec.Name,
		CreatedAt:        ts(rec.CreatedAt),
		Kind:             toProtoProjectKind(rec.Kind),
		SystemKey:        rec.SystemKey,
		Deletion:         toProtoDeletion(rec.Deletion),
		LogRetentionDays: rec.LogRetentionDays,
	}
}

func toProtoDeletion(deletion *deliverycore.DeletionInfo) *platformv1.DeletionState {
	if deletion == nil {
		return nil
	}
	return &platformv1.DeletionState{
		DeletedAt:       ts(deletion.DeletedAt),
		DeletedByUserId: deletion.DeletedByUserID,
		DeleteExpiresAt: ts(deletion.ExpiresAt),
		Inherited:       deletion.Inherited,
	}
}

func toProtoDeletionPreview(preview deletionPreview) *platformv1.DeletionPreview {
	out := &platformv1.DeletionPreview{
		Environments: make([]*platformv1.DeletionPreviewEnvironment, 0, len(preview.Environments)),
		Services:     make([]*platformv1.DeletionPreviewService, 0, len(preview.Services)),
		Domains:      make([]*platformv1.DeletionPreviewDomain, 0, len(preview.Domains)),
		Volumes:      make([]*platformv1.DeletionPreviewVolume, 0, len(preview.Volumes)),
	}
	for _, item := range preview.Environments {
		out.Environments = append(out.Environments, &platformv1.DeletionPreviewEnvironment{
			Id: item.ID, Name: item.Name, IsProduction: item.IsProduction,
		})
	}
	for _, item := range preview.Services {
		out.Services = append(out.Services, &platformv1.DeletionPreviewService{
			Id: item.ID, Name: item.Name, EnvironmentId: item.EnvironmentID, EnvironmentName: item.EnvironmentName,
		})
	}
	for _, item := range preview.Domains {
		out.Domains = append(out.Domains, &platformv1.DeletionPreviewDomain{
			Hostname: item.Hostname, ServiceId: item.ServiceID, ServiceName: item.ServiceName, PlatformGenerated: item.PlatformGenerated,
		})
	}
	for _, item := range preview.Volumes {
		out.Volumes = append(out.Volumes, &platformv1.DeletionPreviewVolume{
			Id: item.ID, Name: item.Name, EnvironmentId: item.EnvironmentID,
		})
	}
	return out
}

func toProtoProjectKind(kind deliverycore.ProjectKind) platformv1.ProjectKind {
	switch kind {
	case deliverycore.ProjectKindManaged:
		return platformv1.ProjectKind_PROJECT_KIND_MANAGED
	default:
		return platformv1.ProjectKind_PROJECT_KIND_USER
	}
}

func toProtoEnvironment(rec deliverycore.EnvironmentRecord) *platformv1.Environment {
	return &platformv1.Environment{
		Id:                      rec.ID,
		ProjectId:               rec.ProjectID,
		Name:                    rec.Name,
		Kind:                    platformv1.EnvironmentKind_ENVIRONMENT_KIND_PERSISTENT,
		IsProduction:            rec.IsProduction,
		AutoDeploy:              rec.AutoDeploy,
		CopiedFromEnvironmentId: rec.CopiedFromEnvironmentID,
		CreatedAt:               ts(rec.CreatedAt),
		UpdatedAt:               ts(rec.UpdatedAt),
		Deletion:                toProtoDeletion(rec.Deletion),
	}
}

func toProtoService(rec deliverycore.ServiceRecord) *platformv1.Service {
	return &platformv1.Service{
		Id:                      rec.ID,
		EnvironmentId:           rec.EnvironmentID,
		Name:                    rec.Name,
		Spec:                    rec.Spec,
		SpecRevision:            rec.SpecRevision,
		CreatedAt:               ts(rec.CreatedAt),
		UpdatedAt:               ts(rec.UpdatedAt),
		RolloutGeneration:       rec.RolloutGeneration,
		SourceSummary:           rec.SourceSummary,
		LastSuccessfulCommitSha: rec.LastSuccessfulCommitSHA,
		ResolvedImage:           rec.ResolvedImage,
		LatestBuild:             rec.LatestBuild,
		PendingChanges:          rec.PendingChanges,
		UnappliedChangeCount:    int32(len(rec.UnappliedChanges)),
		UnappliedChanges:        rec.UnappliedChanges,
		InternalHostname:        deliverycore.InternalServiceHostname(rec.Name, rec.ID),
		LatestDeployment:        toProtoDeploymentStatus(rec.LatestDeployment),
		DesiredReplicaCount:     rec.DesiredReplicaCount,
		ReadyReplicaCount:       rec.ReadyReplicaCount,
		PlacementMessage:        rec.PlacementMessage,
		Deletion:                toProtoDeletion(rec.Deletion),
	}
}

func toProtoServiceWithAllocations(rec deliverycore.ServiceRecord, allocations []deliverycore.AllocationRecord) *platformv1.Service {
	rec.ReadyReplicaCount = countReadyAllocations(allocations)
	if rec.DesiredReplicaCount <= 0 && rec.RolloutGeneration > 0 {
		rec.DesiredReplicaCount = deliverycore.DefaultDesiredReplicaCount
	}
	return toProtoService(rec)
}

func toProtoServiceStatus(rec deliverycore.ServiceRecord, allocations []deliverycore.AllocationRecord, index int64) *platformv1.ServiceStatus {
	return &platformv1.ServiceStatus{
		Service:     toProtoServiceWithAllocations(rec, allocations),
		Allocation:  toProtoAllocation(primaryAllocation(allocations)),
		Allocations: toProtoAllocations(allocations),
		Index:       index,
	}
}

type livePositionReader interface {
	LivePosition() deliverycore.LivePosition
}

func liveReadMeta(d livePositionReader) *platformv1.LiveReadMeta {
	if d == nil {
		return nil
	}
	return toProtoLiveRead(d.LivePosition())
}

func toProtoLiveRead(pos deliverycore.LivePosition) *platformv1.LiveReadMeta {
	meta := &platformv1.LiveReadMeta{
		AcceptedDurablePosition: pos.AcceptedDurable,
		AppliedLivePosition:     pos.AppliedLive,
		LiveOwnerReady:          pos.Ready,
	}
	meta.ObservationFreshness = deliverycore.ToProtoTimestamp(pos.ObservationFreshness)
	return meta
}

func toProtoDomainBinding(rec deliverycore.DomainBindingRecord) *platformv1.DomainBinding {
	return &platformv1.DomainBinding{
		Hostname:          rec.Hostname,
		ServiceId:         rec.ServiceID,
		TargetPort:        rec.TargetPort,
		PlatformGenerated: rec.PlatformGenerated,
		CreatedAt:         ts(rec.CreatedAt),
		UpdatedAt:         ts(rec.UpdatedAt),
		Deletion:          toProtoDeletion(rec.Deletion),
	}
}

func toProtoVolume(rec deliverycore.VolumeRecord) *platformv1.Volume {
	return &platformv1.Volume{
		Id:            rec.ID,
		EnvironmentId: rec.EnvironmentID,
		Name:          rec.Name,
		SizeBytes:     rec.SizeBytes,
		CreatedAt:     ts(rec.CreatedAt),
		Deletion:      toProtoDeletion(rec.Deletion),
	}
}

func toProtoAgent(rec deliverycore.AgentRecord) *platformv1.Agent {
	out := &platformv1.Agent{
		Id:                         rec.ID,
		Name:                       rec.Name,
		AdvertiseAddr:              rec.AdvertiseAddr,
		WireguardEndpoint:          rec.WireGuardEndpoint,
		Healthy:                    rec.Healthy(time.Now().UTC()),
		CpuMillisCapacity:          rec.CPUMillisCapacity,
		MemoryMebibytesCapacity:    rec.MemoryMebibytesCapcity,
		LastSeenAt:                 ts(rec.LastSeenAt),
		LifecycleState:             lifecycleStateProto(rec.LifecycleState),
		Region:                     rec.Region,
		Zone:                       rec.Zone,
		FailureDomain:              rec.FailureDomain,
		ReservedCpuMillis:          rec.ReservedCPUMillis,
		ReservedMemoryMebibytes:    rec.ReservedMemoryMebibytes,
		SchedulableCpuMillis:       max(rec.CPUMillisCapacity-rec.ReservedCPUMillis, 0),
		SchedulableMemoryMebibytes: max(rec.MemoryMebibytesCapcity-rec.ReservedMemoryMebibytes, 0),
		RuntimeCapabilities:        append([]string(nil), rec.RuntimeCapabilities...),
		SoftwareVersion:            rec.SoftwareVersion,
		MaintenanceMessage:         rec.MaintenanceMessage,
	}
	if rec.CredentialRevokedAt.Valid {
		out.CredentialRevokedAt = ts(rec.CredentialRevokedAt.Time)
	}
	return out
}

func toProtoAllocation(rec deliverycore.AllocationRecord) *platformv1.AllocationStatus {
	out := &platformv1.AllocationStatus{
		AllocationId:             rec.ID,
		ServiceId:                rec.ServiceID,
		AgentId:                  rec.AgentID,
		DesiredSpecRevision:      rec.DesiredSpecRevision,
		AppliedSpecRevision:      rec.AppliedSpecRevision,
		Phase:                    rec.Phase,
		Message:                  rec.Message,
		AllocationIpv4:           rec.AllocationIPv4,
		AllocationIpv6:           rec.AllocationIPv6,
		Healthy:                  rec.Healthy,
		HealthyIpv4Ports:         append([]int32(nil), rec.HealthyIPv4Ports...),
		HealthyIpv6Ports:         append([]int32(nil), rec.HealthyIPv6Ports...),
		UpdatedAt:                ts(rec.UpdatedAt),
		DesiredRolloutGeneration: rec.DesiredRolloutGeneration,
		AppliedRolloutGeneration: rec.AppliedRolloutGeneration,
		Restart:                  rec.Restart,
		OperatorRestartNonce:     rec.OperatorRestartNonce,
		RolloutState:             rec.RolloutState,
	}
	if rec.DrainStartedAt.Valid {
		out.DrainStartedAt = ts(rec.DrainStartedAt.Time)
	}
	if rec.DrainDeadline.Valid {
		out.DrainDeadline = ts(rec.DrainDeadline.Time)
	}
	return out
}

func toProtoAllocations(recs []deliverycore.AllocationRecord) []*platformv1.AllocationStatus {
	if len(recs) == 0 {
		return nil
	}
	out := make([]*platformv1.AllocationStatus, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toProtoAllocation(rec))
	}
	return out
}

func primaryAllocation(recs []deliverycore.AllocationRecord) deliverycore.AllocationRecord {
	if len(recs) == 0 {
		return deliverycore.AllocationRecord{}
	}
	return recs[0]
}

func toProtoServiceLogLine(rec logs.ServiceLog) *platformv1.ServiceLogLine {
	return &platformv1.ServiceLogLine{
		ObservedAt:        ts(rec.ObservedAt),
		EnvironmentId:     rec.EnvironmentID,
		ServiceId:         rec.ServiceID,
		AllocationId:      rec.AllocationID,
		AgentId:           rec.AgentID,
		Stream:            rec.Stream,
		RolloutGeneration: rec.RolloutGeneration,
		Sequence:          rec.Sequence,
		Line:              rec.Line,
		LogType:           logs.TypeToProto(rec.LogType),
		BuildId:           rec.BuildID,
		Stage:             rec.Stage,
		Attributes:        rec.Attributes,
		Truncated:         rec.Truncated,
		Event:             rec.Event,
		LineId:            rec.LineID,
	}
}

func toProtoServiceLogGap(gap logs.ServiceLogGap) *platformv1.ServiceLogGap {
	return &platformv1.ServiceLogGap{
		AllocationId: gap.AllocationID,
		BuildId:      gap.BuildID,
		LogType:      logs.TypeToProto(gap.LogType),
		Stream:       gap.Stream,
		DroppedCount: gap.DroppedCount,
		Reason:       gap.Reason,
		WindowStart:  ts(gap.WindowStart),
		WindowEnd:    ts(gap.WindowEnd),
	}
}

func toProtoDeploymentRecord(rec deliverycore.DeploymentRecord) *platformv1.DeploymentRecord {
	status := toProtoDeploymentStatus(&rec)
	protoRec := &platformv1.DeploymentRecord{
		Id:                rec.ID,
		ServiceId:         rec.ServiceID,
		RolloutGeneration: rec.RolloutGeneration,
		SpecRevision:      rec.SpecRevision,
		CreatedAt:         ts(rec.CreatedAt),
		Build:             toProtoMaybeBuildStatus(rec.Build),
		IsCurrent:         rec.IsCurrent,
		RequestedByUserId: rec.RequestedByUserID,
		Status:            status,
		ImageDigest:       rec.ImageDigest,
		Artifact:          deliverycore.ToProtoBuildArtifact(rec.Artifact),
	}
	for _, action := range rec.Actions {
		protoRec.Actions = append(protoRec.Actions, &platformv1.DeploymentActionRecord{
			Id: action.ID, Action: deliverycore.ToProtoDeploymentAction(action.Action),
			TargetDeploymentId: action.TargetDeploymentID, ResultDeploymentId: action.ResultDeploymentID,
			AllocationId: action.AllocationID, RequestedByUserId: action.RequestedByUserID,
			CreatedAt: ts(action.CreatedAt),
		})
	}
	protoRec.Stages = deploymentStagesFromLifecycle(rec, deliverycore.ServiceRecord{ID: rec.ServiceID, SpecRevision: rec.SpecRevision}, rec.Build)
	return protoRec
}

func toProtoDeploymentStatus(rec *deliverycore.DeploymentRecord) *platformv1.DeploymentStatus {
	if rec == nil || rec.ID == "" && rec.State == "" {
		return nil
	}
	return &platformv1.DeploymentStatus{
		DeploymentId:      rec.ID,
		State:             toProtoDeploymentState(rec.State),
		TransitionedAt:    ts(rec.UpdatedAt),
		CauseKind:         toProtoDeploymentCauseKind(rec.CauseKind),
		CauseId:           rec.CauseID,
		ReasonCode:        rec.ReasonCode,
		Detail:            rec.Detail,
		SpecRevision:      rec.SpecRevision,
		ImageDigest:       rec.ImageDigest,
		RolloutGeneration: rec.RolloutGeneration,
		BuildReused:       rec.BuildReused(),
	}
}

func toProtoMaybeBuildStatus(rec *deliverycore.BuildRunRecord) *platformv1.BuildStatus {
	if rec == nil {
		return nil
	}
	return deliverycore.ToProtoBuildStatus(*rec)
}

func ts(v time.Time) *timestamppb.Timestamp {
	return deliverycore.ToProtoTimestamp(v)
}

func deploymentStages(service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) []*platformv1.DeploymentStage {
	if service.LatestDeployment == nil {
		return nil
	}
	return deploymentStagesFromLifecycle(*service.LatestDeployment, service, build)
}

func deploymentStagesFromLifecycle(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) []*platformv1.DeploymentStage {
	stages := make([]*platformv1.DeploymentStage, 0, 4)
	if includeInitializationStage(service, build) && dep.State != deliverycore.DeploymentStateQueuedBuild && dep.State != deliverycore.DeploymentStateBuilding {
		stages = append(stages, lifecycleInitializationStage(dep, service))
	}
	stages = append(stages, lifecycleBuildStage(dep, service, build))
	stages = append(stages, lifecycleDeployStage(dep, service, build))
	stages = append(stages, lifecyclePostDeployStage(dep, build))
	return stages
}

func includeInitializationStage(service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) bool {
	return !(source.DesiredSourceSpec(service.Spec) != nil && build != nil)
}

func lifecycleInitializationStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:       logs.StageInitialization,
		Label:     "Initialization",
		StartedAt: ts(firstTransitionTime(dep, deliverycore.DeploymentStateStaged, dep.CreatedAt)),
	}
	switch dep.State {
	case deliverycore.DeploymentStateStaged:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Selecting a host")
	case deliverycore.DeploymentStateFailed:
		if !passedState(dep, deliverycore.DeploymentStateScheduling) {
			stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
			stage.Detail = firstNonEmpty(dep.Detail, "Initialization failed")
			stage.FinishedAt = ts(dep.UpdatedAt)
			break
		}
		fallthrough
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deliverycore.DeploymentStateScheduling), "Service scheduled")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Service scheduled on " + service.AllocatedAgentID
		}
		finished := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.UpdatedAt)
		if finished.IsZero() {
			finished = dep.CreatedAt
		}
		stage.FinishedAt = ts(finished)
	}
	return stage
}

func lifecycleBuildStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StageBuild,
		Label: "Build",
	}
	if source.DesiredSourceSpec(service.Spec) == nil && dep.BuildID == "" && build == nil {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "No build required — using prebuilt image"
		return stage
	}
	if dep.BuildReused() {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = "Reusing image built for commit"
		if build != nil && build.CommitSHA != "" {
			stage.Detail += " " + shortSHA(build.CommitSHA)
		}
		return stage
	}
	started := firstTransitionTime(dep, deliverycore.DeploymentStateQueuedBuild, time.Time{})
	if started.IsZero() && build != nil {
		started = build.QueuedAt
		if build.StartedAt.Valid {
			started = build.StartedAt.Time
		}
	}
	if !started.IsZero() {
		stage.StartedAt = ts(started)
	}
	switch {
	case dep.State == deliverycore.DeploymentStateQueuedBuild:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for a builder")
	case dep.State == deliverycore.DeploymentStateBuilding:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Building the image…")
	case dep.State == deliverycore.DeploymentStateFailed && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, buildFailureDetail(build), "Build failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateSuperseded && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = firstNonEmpty(dep.Detail, "Superseded by a newer build")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateCancelled && !passedState(dep, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Build cancelled")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case passedState(dep, deliverycore.DeploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateScheduling):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = "Image ready"
		finished := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.UpdatedAt)
		if build != nil && build.FinishedAt.Valid {
			finished = build.FinishedAt.Time
		}
		stage.FinishedAt = ts(finished)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for source"
	}
	return stage
}

func lifecycleDeployStage(dep deliverycore.DeploymentRecord, service deliverycore.ServiceRecord, build *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StageDeploy,
		Label: "Deploy",
	}
	if !reachedDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for build to finish"
		return stage
	}
	started := firstTransitionTime(dep, deliverycore.DeploymentStateScheduling, dep.CreatedAt)
	if build != nil && build.FinishedAt.Valid {
		started = build.FinishedAt.Time
	}
	stage.StartedAt = ts(started)
	switch {
	case dep.State == deliverycore.DeploymentStateFailed && !passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateCrashed && !passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Deploy failed")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case dep.State == deliverycore.DeploymentStateScheduling, dep.State == deliverycore.DeploymentStateImagePull, dep.State == deliverycore.DeploymentStateStarting:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, deliverycore.DefaultDetailForState(dep.State))
	case deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateReadiness) || passedState(dep, deliverycore.DeploymentStateReadiness):
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deliverycore.DeploymentStateStarting), "Running")
		if service.AllocatedAgentID != "" {
			stage.Detail = "Running on " + service.AllocatedAgentID
		}
		stage.FinishedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateReadiness, dep.UpdatedAt))
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for scheduler"
	}
	return stage
}

func lifecyclePostDeployStage(dep deliverycore.DeploymentRecord, _ *deliverycore.BuildRunRecord) *platformv1.DeploymentStage {
	stage := &platformv1.DeploymentStage{
		Key:   logs.StagePostDeploy,
		Label: "Post-deploy",
	}
	if !reachedPostDeployPhase(dep) {
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_PENDING
		stage.Detail = "Waiting for rollout"
		return stage
	}
	stage.StartedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateReadiness, dep.UpdatedAt))
	switch dep.State {
	case deliverycore.DeploymentStateReadiness:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_FAILED
		stage.Detail = firstNonEmpty(dep.Detail, "Workload unhealthy")
		stage.FinishedAt = ts(dep.UpdatedAt)
	case deliverycore.DeploymentStateActive, deliverycore.DeploymentStateDraining, deliverycore.DeploymentStateCompleted:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SUCCEEDED
		stage.Detail = firstNonEmpty(detailForState(dep, deliverycore.DeploymentStateActive), "Deployment ready")
		stage.FinishedAt = ts(firstTransitionTime(dep, deliverycore.DeploymentStateActive, dep.UpdatedAt))
	case deliverycore.DeploymentStateCancelled, deliverycore.DeploymentStateRemoved, deliverycore.DeploymentStateSuperseded:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_SKIPPED
		stage.Detail = firstNonEmpty(dep.Detail, deliverycore.DefaultDetailForState(dep.State))
		stage.FinishedAt = ts(dep.UpdatedAt)
	default:
		stage.State = platformv1.DeploymentStageState_DEPLOYMENT_STAGE_STATE_RUNNING
		stage.Detail = firstNonEmpty(dep.Detail, "Waiting for HTTP readiness check")
	}
	return stage
}

func passedState(dep deliverycore.DeploymentRecord, state string) bool {
	if dep.State == state {
		return true
	}
	for _, transition := range dep.Transitions {
		if transition.ToState == state {
			return true
		}
	}
	return false
}

func firstTransitionTime(dep deliverycore.DeploymentRecord, state string, fallback time.Time) time.Time {
	for _, transition := range dep.Transitions {
		if transition.ToState == state {
			return transition.OccurredAt
		}
	}
	if dep.State == state {
		return dep.UpdatedAt
	}
	return fallback
}

func detailForState(dep deliverycore.DeploymentRecord, state string) string {
	for _, transition := range dep.Transitions {
		if transition.ToState == state && transition.Detail != "" {
			return transition.Detail
		}
	}
	if dep.State == state {
		return dep.Detail
	}
	return ""
}

func buildFailureDetail(build *deliverycore.BuildRunRecord) string {
	if build == nil {
		return ""
	}
	return build.FailureReason
}

func reachedDeployPhase(dep deliverycore.DeploymentRecord) bool {
	if passedState(dep, deliverycore.DeploymentStateScheduling) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateScheduling) {
		return true
	}
	switch dep.State {
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed, deliverycore.DeploymentStateCancelled, deliverycore.DeploymentStateRemoved:
		return dep.RolloutGeneration > 0 && dep.BuildID == ""
	default:
		return false
	}
}

func reachedPostDeployPhase(dep deliverycore.DeploymentRecord) bool {
	if passedState(dep, deliverycore.DeploymentStateReadiness) || deploymentProgressAtLeast(dep.State, deliverycore.DeploymentStateReadiness) {
		return true
	}
	switch dep.State {
	case deliverycore.DeploymentStateFailed, deliverycore.DeploymentStateCrashed:
		return dep.RolloutGeneration > 0
	default:
		return false
	}
}

func deploymentProgressAtLeast(state, min string) bool {
	order := []string{
		deliverycore.DeploymentStateStaged,
		deliverycore.DeploymentStateQueuedBuild,
		deliverycore.DeploymentStateBuilding,
		deliverycore.DeploymentStateScheduling,
		deliverycore.DeploymentStateImagePull,
		deliverycore.DeploymentStateStarting,
		deliverycore.DeploymentStateReadiness,
		deliverycore.DeploymentStateActive,
		deliverycore.DeploymentStateDraining,
		deliverycore.DeploymentStateCompleted,
	}
	stateIdx := slices.Index(order, state)
	minIdx := slices.Index(order, min)
	return stateIdx >= 0 && minIdx >= 0 && stateIdx >= minIdx
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func toProtoDeploymentState(state string) platformv1.DeploymentState {
	switch state {
	case deliverycore.DeploymentStateStaged:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STAGED
	case deliverycore.DeploymentStateQueuedBuild:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_QUEUED_BUILD
	case deliverycore.DeploymentStateBuilding:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_BUILDING
	case deliverycore.DeploymentStateScheduling:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SCHEDULING
	case deliverycore.DeploymentStateImagePull:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_IMAGE_PULL
	case deliverycore.DeploymentStateStarting:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_STARTING
	case deliverycore.DeploymentStateReadiness:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_READINESS
	case deliverycore.DeploymentStateActive:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_ACTIVE
	case deliverycore.DeploymentStateDraining:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_DRAINING
	case deliverycore.DeploymentStateCompleted:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_COMPLETED
	case deliverycore.DeploymentStateFailed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_FAILED
	case deliverycore.DeploymentStateCancelled:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CANCELLED
	case deliverycore.DeploymentStateCrashed:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_CRASHED
	case deliverycore.DeploymentStateRemoved:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_REMOVED
	case deliverycore.DeploymentStateSuperseded:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_SUPERSEDED
	default:
		return platformv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
}

func toProtoDeploymentCauseKind(kind string) platformv1.DeploymentCauseKind {
	switch kind {
	case deliverycore.DeploymentCauseUser:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_USER
	case deliverycore.DeploymentCauseSystem:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_SYSTEM
	case deliverycore.DeploymentCauseAgent:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_AGENT
	case deliverycore.DeploymentCauseBuilder:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_BUILDER
	case deliverycore.DeploymentCauseWebhook:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_WEBHOOK
	default:
		return platformv1.DeploymentCauseKind_DEPLOYMENT_CAUSE_KIND_UNSPECIFIED
	}
}

func countReadyAllocations(recs []deliverycore.AllocationRecord) int32 {
	var ready int32
	for _, rec := range recs {
		if deliverycore.AllocationReady(rec) {
			ready++
		}
	}
	return ready
}
