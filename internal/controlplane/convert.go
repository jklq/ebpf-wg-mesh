package controlplane

import (
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

func toProtoPrincipal(rec principalRecord) *platformv1.Principal {
	return &platformv1.Principal{
		Subject:   rec.Subject,
		Email:     rec.Email,
		CreatedAt: ts(rec.CreatedAt),
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

func toProtoService(rec serviceRecord) *platformv1.Service {
	return &platformv1.Service{
		Id:                rec.ID,
		ProjectId:         rec.ProjectID,
		Name:              rec.Name,
		Spec:              rec.Spec,
		SpecRevision:      rec.SpecRevision,
		AllocatedAgentId:  rec.AllocatedAgentID,
		CreatedAt:         ts(rec.CreatedAt),
		UpdatedAt:         ts(rec.UpdatedAt),
		RolloutGeneration: rec.RolloutGeneration,
	}
}

func toProtoDomainBinding(rec domainBindingRecord) *platformv1.DomainBinding {
	return &platformv1.DomainBinding{
		Hostname:  rec.Hostname,
		ProjectId: rec.ProjectID,
		ServiceId: rec.ServiceID,
		CreatedAt: ts(rec.CreatedAt),
		UpdatedAt: ts(rec.UpdatedAt),
	}
}

func toProtoVolume(rec volumeRecord) *platformv1.Volume {
	return &platformv1.Volume{
		Id:           rec.ID,
		ProjectId:    rec.ProjectID,
		Name:         rec.Name,
		SizeBytes:    rec.SizeBytes,
		BoundAgentId: rec.BoundAgentID,
		CreatedAt:    ts(rec.CreatedAt),
	}
}

func toProtoAgent(rec agentRecord) *platformv1.Agent {
	return &platformv1.Agent{
		Id:                      rec.ID,
		Name:                    rec.Name,
		AdvertiseAddr:           rec.AdvertiseAddr,
		Healthy:                 rec.healthy(time.Now().UTC()),
		CpuMillisCapacity:       rec.CPUMillisCapacity,
		MemoryMebibytesCapacity: rec.MemoryMebibytesCapcity,
		LastSeenAt:              ts(rec.LastSeenAt),
	}
}

func toProtoAllocation(rec allocationRecord) *platformv1.AllocationStatus {
	return &platformv1.AllocationStatus{
		AllocationId:             rec.ID,
		ServiceId:                rec.ServiceID,
		AgentId:                  rec.AgentID,
		DesiredSpecRevision:      rec.DesiredSpecRevision,
		AppliedSpecRevision:      rec.AppliedSpecRevision,
		Phase:                    rec.Phase,
		Message:                  rec.Message,
		EndpointAddr:             rec.EndpointAddr,
		Healthy:                  rec.Healthy,
		UpdatedAt:                ts(rec.UpdatedAt),
		DesiredRolloutGeneration: rec.DesiredRolloutGeneration,
		AppliedRolloutGeneration: rec.AppliedRolloutGeneration,
	}
}
