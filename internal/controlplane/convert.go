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
	}
}

func toProtoService(rec serviceRecord) *platformv1.Service {
	return &platformv1.Service{
		Id:               rec.ID,
		ProjectId:        rec.ProjectID,
		Name:             rec.Name,
		Spec:             rec.Spec,
		CurrentRevision:  rec.CurrentRevision,
		AllocatedAgentId: rec.AllocatedAgentID,
		Domains:          rec.Domains,
		CreatedAt:        ts(rec.CreatedAt),
		UpdatedAt:        ts(rec.UpdatedAt),
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
		AllocationId:    rec.ID,
		ServiceId:       rec.ServiceID,
		AgentId:         rec.AgentID,
		DesiredRevision: rec.DesiredRevision,
		AppliedRevision: rec.AppliedRevision,
		Phase:           rec.Phase,
		Message:         rec.Message,
		EndpointAddr:    rec.EndpointAddr,
		Healthy:         rec.Healthy,
		UpdatedAt:       ts(rec.UpdatedAt),
	}
}
