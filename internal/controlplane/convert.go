package controlplane

import (
	"google.golang.org/protobuf/proto"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func toProtoProject(rec projectRecord) *platformv1.Project {
	return &platformv1.Project{
		Id:        rec.ID,
		Name:      rec.Name,
		CreatedAt: ts(rec.CreatedAt),
		Kind:      toProtoProjectKind(rec.Kind),
		SystemKey: rec.SystemKey,
	}
}

func toProtoProjectKind(kind projectKind) platformv1.ProjectKind {
	switch kind {
	case projectKindManaged:
		return platformv1.ProjectKind_PROJECT_KIND_MANAGED
	default:
		return platformv1.ProjectKind_PROJECT_KIND_USER
	}
}

func toProtoEnvironment(rec environmentRecord) *platformv1.Environment {
	return &platformv1.Environment{
		Id:                      rec.ID,
		ProjectId:               rec.ProjectID,
		Name:                    rec.Name,
		Kind:                    platformv1.EnvironmentKind_ENVIRONMENT_KIND_PERSISTENT,
		IsProduction:            rec.IsProduction,
		CopiedFromEnvironmentId: rec.CopiedFromEnvironmentID,
		CreatedAt:               ts(rec.CreatedAt),
		UpdatedAt:               ts(rec.UpdatedAt),
	}
}

func toProtoService(rec serviceRecord) *platformv1.Service {
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
		InternalHostname:        internalServiceHostname(rec.Name, rec.ID),
		LatestDeployment:        toProtoDeploymentStatus(rec.LatestDeployment),
		DesiredReplicaCount:     rec.DesiredReplicaCount,
		ReadyReplicaCount:       rec.ReadyReplicaCount,
		PlacementMessage:        rec.PlacementMessage,
	}
}

func toProtoServiceWithAllocations(rec serviceRecord, allocations []allocationRecord) *platformv1.Service {
	rec.ReadyReplicaCount = countReadyAllocations(allocations)
	if rec.DesiredReplicaCount <= 0 && rec.RolloutGeneration > 0 {
		rec.DesiredReplicaCount = defaultDesiredReplicaCount
	}
	return toProtoService(rec)
}

func toProtoServiceStatus(rec serviceRecord, allocations []allocationRecord, index int64) *platformv1.ServiceStatus {
	return &platformv1.ServiceStatus{
		Service:     toProtoServiceWithAllocations(rec, allocations),
		Allocation:  toProtoAllocation(primaryAllocation(allocations)),
		Allocations: toProtoAllocations(allocations),
		Index:       index,
	}
}

func toProtoDomainBinding(rec domainBindingRecord) *platformv1.DomainBinding {
	return &platformv1.DomainBinding{
		Hostname:          rec.Hostname,
		ServiceId:         rec.ServiceID,
		TargetPort:        rec.TargetPort,
		PlatformGenerated: rec.PlatformGenerated,
		CreatedAt:         ts(rec.CreatedAt),
		UpdatedAt:         ts(rec.UpdatedAt),
	}
}

func toProtoVolume(rec volumeRecord) *platformv1.Volume {
	return &platformv1.Volume{
		Id:            rec.ID,
		EnvironmentId: rec.EnvironmentID,
		Name:          rec.Name,
		SizeBytes:     rec.SizeBytes,
		CreatedAt:     ts(rec.CreatedAt),
	}
}

func toProtoAgent(rec agentRecord) *platformv1.Agent {
	out := &platformv1.Agent{
		Id:                         rec.ID,
		Name:                       rec.Name,
		AdvertiseAddr:              rec.AdvertiseAddr,
		Healthy:                    rec.healthy(time.Now().UTC()),
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

func toProtoAllocation(rec allocationRecord) *platformv1.AllocationStatus {
	out := &platformv1.AllocationStatus{
		AllocationId:             rec.ID,
		ServiceId:                rec.ServiceID,
		AgentId:                  rec.AgentID,
		DesiredSpecRevision:      rec.DesiredSpecRevision,
		AppliedSpecRevision:      rec.AppliedSpecRevision,
		Phase:                    rec.Phase,
		Message:                  rec.Message,
		AllocationIp:             rec.AllocationIP,
		Healthy:                  rec.Healthy,
		HealthyPorts:             append([]int32(nil), rec.HealthyPorts...),
		UpdatedAt:                ts(rec.UpdatedAt),
		DesiredRolloutGeneration: rec.DesiredRolloutGeneration,
		AppliedRolloutGeneration: rec.AppliedRolloutGeneration,
		Restart:                  rec.Restart,
		OperatorRestartNonce:     rec.OperatorRestartNonce,
	}
	return out
}

func toProtoAllocations(recs []allocationRecord) []*platformv1.AllocationStatus {
	if len(recs) == 0 {
		return nil
	}
	out := make([]*platformv1.AllocationStatus, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toProtoAllocation(rec))
	}
	return out
}

func primaryAllocation(recs []allocationRecord) allocationRecord {
	if len(recs) == 0 {
		return allocationRecord{}
	}
	return recs[0]
}

func toProtoServiceLogLine(rec serviceLogRecord) *platformv1.ServiceLogLine {
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
		LogType:           logTypeToProto(rec.LogType),
		BuildId:           rec.BuildID,
		Stage:             rec.Stage,
	}
}

func toProtoBuildStatus(rec buildRunRecord) *platformv1.BuildStatus {
	if rec.ID == "" {
		return nil
	}
	status := &platformv1.BuildStatus{
		BuildId:       rec.ID,
		State:         toProtoBuildState(rec.State),
		CommitSha:     rec.CommitSHA,
		ImageDigest:   rec.ImageDigest,
		QueuedAt:      ts(rec.QueuedAt),
		FailureReason: rec.FailureReason,
		CommitMessage: rec.CommitMessage,
		CommitAuthor:  rec.CommitAuthor,
	}
	if rec.StartedAt.Valid {
		status.StartedAt = ts(rec.StartedAt.Time)
	}
	if rec.FinishedAt.Valid {
		status.FinishedAt = ts(rec.FinishedAt.Time)
	}
	return status
}

func toProtoDeploymentRecord(rec deploymentRecord) *platformv1.DeploymentRecord {
	status := toProtoDeploymentStatus(&rec)
	protoRec := &platformv1.DeploymentRecord{
		Id:                rec.ID,
		ServiceId:         rec.ServiceID,
		RolloutGeneration: rec.RolloutGeneration,
		SpecRevision:      rec.SpecRevision,
		Reason:            rec.Reason,
		CreatedAt:         ts(rec.CreatedAt),
		Build:             toProtoMaybeBuildStatus(rec.Build),
		IsCurrent:         rec.IsCurrent,
		RequestedByUserId: rec.RequestedByUserID,
		Status:            status,
		ImageDigest:       rec.ImageDigest,
	}
	if rec.Build != nil {
		protoRec.Stages = deploymentStagesFromLifecycle(rec, serviceRecord{ID: rec.ServiceID, SpecRevision: rec.SpecRevision, AllocatedAgentID: ""}, rec.Build)
	} else {
		protoRec.Stages = deploymentStagesFromLifecycle(rec, serviceRecord{ID: rec.ServiceID, SpecRevision: rec.SpecRevision}, nil)
	}
	return protoRec
}

func toProtoDeploymentStatus(rec *deploymentRecord) *platformv1.DeploymentStatus {
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
	}
}

func toProtoMaybeBuildStatus(rec *buildRunRecord) *platformv1.BuildStatus {
	if rec == nil {
		return nil
	}
	return toProtoBuildStatus(*rec)
}

func toProtoBuildState(state string) platformv1.BuildState {
	switch state {
	case "queued":
		return platformv1.BuildState_BUILD_STATE_QUEUED
	case "running":
		return platformv1.BuildState_BUILD_STATE_RUNNING
	case "succeeded":
		return platformv1.BuildState_BUILD_STATE_SUCCEEDED
	case "failed":
		return platformv1.BuildState_BUILD_STATE_FAILED
	case "superseded":
		return platformv1.BuildState_BUILD_STATE_SUPERSEDED
	default:
		return platformv1.BuildState_BUILD_STATE_UNSPECIFIED
	}
}

func toProtoResolvedSourceBinding(rec sourceBindingRecord) *platformv1.ResolvedSourceBinding {
	if rec.ID == "" {
		return nil
	}
	return &platformv1.ResolvedSourceBinding{
		Id:                           rec.ID,
		ServiceId:                    rec.ServiceID,
		Provider:                     rec.Provider,
		RepositorySelector:           rec.RepositorySelector,
		TrackedRef:                   rec.TrackedRef,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		ProviderScopeExternalId:      rec.ProviderScopeExternalID,
		AccessState:                  toProtoSourceAccessState(rec.AccessState),
		ResolvedAt:                   ts(rec.ResolvedAt),
		FreshUntil:                   ts(rec.FreshUntil),
		BuildRecipe:                  cloneBuildRecipe(rec.BuildRecipe),
	}
}

func toProtoSourceRevision(rec sourceRevisionRecord) *platformv1.SourceRevision {
	if rec.ID == "" {
		return nil
	}
	return &platformv1.SourceRevision{
		Id:                           rec.ID,
		SourceBindingId:              rec.SourceBindingID,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		TrackedRef:                   rec.TrackedRef,
		CommitSha:                    rec.CommitSHA,
		ObservedAt:                   ts(rec.ObservedAt),
	}
}

func toProtoSourceSnapshot(rec sourceSnapshotRecord) *platformv1.SourceSnapshot {
	if rec.ID == "" {
		return nil
	}
	snapshot := &platformv1.SourceSnapshot{
		Id:                           rec.ID,
		SourceRevisionId:             rec.SourceRevisionID,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		CommitSha:                    rec.CommitSHA,
		Digest:                       rec.Digest,
		Ready:                        rec.Ready,
	}
	if rec.FetchedAt.Valid {
		snapshot.FetchedAt = ts(rec.FetchedAt.Time)
	}
	return snapshot
}

func toProtoSourceStateSummary(desired *platformv1.ServiceSourceSpec, binding *sourceBindingRecord, revision *sourceRevisionRecord, snapshot *sourceSnapshotRecord) *platformv1.ServiceSourceSummary {
	if desired == nil {
		return nil
	}
	state := &platformv1.SourceStateSummary{
		DesiredSpec: proto.Clone(desired).(*platformv1.ServiceSourceSpec),
	}
	if binding != nil {
		state.ResolvedBinding = toProtoResolvedSourceBinding(*binding)
	}
	if revision != nil {
		state.LatestRevision = toProtoSourceRevision(*revision)
	}
	if snapshot != nil {
		state.LatestSnapshot = toProtoSourceSnapshot(*snapshot)
	}
	return &platformv1.ServiceSourceSummary{
		Source: &platformv1.ServiceSourceSummary_SourceState{
			SourceState: state,
		},
	}
}
