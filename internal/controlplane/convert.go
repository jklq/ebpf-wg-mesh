package controlplane

import (
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/logs"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func toProtoProject(rec deliverycore.ProjectRecord) *platformv1.Project {
	return &platformv1.Project{
		Id:        rec.ID,
		Name:      rec.Name,
		CreatedAt: ts(rec.CreatedAt),
		Kind:      toProtoProjectKind(rec.Kind),
		SystemKey: rec.SystemKey,
	}
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
	if !pos.ObservationFreshness.IsZero() {
		meta.ObservationFreshness = timestamppb.New(pos.ObservationFreshness)
	}
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
	}
}

func toProtoVolume(rec deliverycore.VolumeRecord) *platformv1.Volume {
	return &platformv1.Volume{
		Id:            rec.ID,
		EnvironmentId: rec.EnvironmentID,
		Name:          rec.Name,
		SizeBytes:     rec.SizeBytes,
		CreatedAt:     ts(rec.CreatedAt),
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
		VariableVersions:  rec.VariableVersions,
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
	}
}

func toProtoMaybeBuildStatus(rec *deliverycore.BuildRunRecord) *platformv1.BuildStatus {
	if rec == nil {
		return nil
	}
	return deliverycore.ToProtoBuildStatus(*rec)
}

func ts(v time.Time) *timestamppb.Timestamp {
	return timestamppb.New(v)
}
