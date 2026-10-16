package mcpserver

import (
	"context"
	"fmt"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mebibyte = 1024 * 1024
	gibibyte = 1024 * mebibyte
)

func (s *state) addDomainVolumeTools(server *mcp.Server) {
	type listDomainsIn struct {
		ServiceID      string `json:"service_id"`
		IncludeDeleted bool   `json:"include_deleted,omitempty"`
	}
	add(server, readTool("list_domains", "List a service's domain bindings with ownership and certificate state."), func(ctx context.Context, in listDomainsIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		return s.client.ListDomains(ctx, id, in.IncludeDeleted)
	})

	type hostnameIn struct {
		Hostname string `json:"hostname"`
	}
	add(server, readTool("domain_status", "Get one domain binding: target, DNS ownership verification, and HTTPS certificate state."), func(ctx context.Context, in hostnameIn) (any, error) {
		if err := required("hostname", in.Hostname); err != nil {
			return nil, err
		}
		return s.client.GetDomain(ctx, in.Hostname)
	})

	type generateDomainIn struct {
		ServiceID  string `json:"service_id"`
		TargetPort int32  `json:"target_port,omitempty" jsonschema:"service port to expose (default: primary port)"`
	}
	add(server, writeTool("generate_domain", "Generate a platform hostname (*.platform suffix) for a service port. At most one generated domain per service."), func(ctx context.Context, in generateDomainIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		port := in.TargetPort
		if port == 0 {
			svc, err := s.client.GetService(ctx, id)
			if err != nil {
				return nil, err
			}
			port = primaryPort(svc)
			if port == 0 {
				return nil, fmt.Errorf("service has no ports; pass target_port or add ports with update_service")
			}
		}
		return s.client.GenerateDomain(ctx, id, port)
	})

	type createDomainIn struct {
		ServiceID  string `json:"service_id"`
		Hostname   string `json:"hostname" jsonschema:"custom hostname; point its DNS at the platform to verify ownership"`
		TargetPort int32  `json:"target_port,omitempty" jsonschema:"default: primary port"`
	}
	add(server, writeTool("create_domain", "Bind a custom hostname to a service port."), func(ctx context.Context, in createDomainIn) (any, error) {
		id := s.serviceID(in.ServiceID)
		if err := required("service_id", id); err != nil {
			return nil, err
		}
		port := in.TargetPort
		if port == 0 {
			svc, err := s.client.GetService(ctx, id)
			if err != nil {
				return nil, err
			}
			port = primaryPort(svc)
			if port == 0 {
				return nil, fmt.Errorf("service has no ports; pass target_port or add ports with update_service")
			}
		}
		return s.client.CreateDomain(ctx, &platformv1.DomainBindingInput{Hostname: in.Hostname, ServiceId: id, TargetPort: port})
	})

	type updateDomainIn struct {
		Hostname   string `json:"hostname"`
		ServiceID  string `json:"service_id" jsonschema:"new target service"`
		TargetPort int32  `json:"target_port" jsonschema:"new target port"`
	}
	add(server, writeTool("update_domain", "Retarget a domain binding to a different service port."), func(ctx context.Context, in updateDomainIn) (any, error) {
		return s.client.UpdateDomain(ctx, in.Hostname, &platformv1.DomainBindingTarget{ServiceId: in.ServiceID, TargetPort: in.TargetPort})
	})

	type deleteDomainIn struct {
		Hostname string `json:"hostname"`
		Confirm  bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("delete_domain", "Delete a domain binding (restorable during grace). Destructive: preview first, then confirm:true."), func(ctx context.Context, in deleteDomainIn) (any, error) {
		if !in.Confirm {
			binding, err := s.client.GetDomain(ctx, in.Hostname)
			if err != nil {
				return nil, err
			}
			return preview(fmt.Sprintf("delete domain binding %s (service %s port %d)", in.Hostname, binding.GetServiceId(), binding.GetTargetPort()), map[string]any{"domain": protoJSON(binding)}), nil
		}
		if err := s.client.DeleteDomain(ctx, in.Hostname); err != nil {
			return nil, err
		}
		return deleted("domain", in.Hostname, ""), nil
	})

	add(server, writeTool("restore_domain", "Restore a deleted domain binding."), func(ctx context.Context, in hostnameIn) (any, error) {
		return s.client.RestoreDomain(ctx, in.Hostname)
	})

	type listVolumesIn struct {
		EnvironmentID  string `json:"environment_id"`
		IncludeDeleted bool   `json:"include_deleted,omitempty"`
	}
	add(server, readTool("list_volumes", "List volumes in an environment with size, usage, pinned node, and state."), func(ctx context.Context, in listVolumesIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		return s.client.ListVolumes(ctx, id, in.IncludeDeleted)
	})

	type createVolumeIn struct {
		EnvironmentID string `json:"environment_id"`
		Name          string `json:"name" jsonschema:"volume name (services mount it by this name)"`
		SizeGib       int64  `json:"size_gib,omitempty" jsonschema:"size in GiB (either size_gib or size_bytes)"`
		SizeBytes     int64  `json:"size_bytes,omitempty"`
	}
	add(server, writeTool("create_volume", "Create a volume in an environment. Single-node, not backed up; staged until the environment is released. Attach it with create_service/update_service volume_name + volume_mount_path."), func(ctx context.Context, in createVolumeIn) (any, error) {
		id := s.environmentID(in.EnvironmentID)
		if err := required("environment_id", id); err != nil {
			return nil, err
		}
		size, err := volumeSize(in.SizeGib, in.SizeBytes)
		if err != nil {
			return nil, err
		}
		created, err := s.client.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: id, Name: in.Name, SizeBytes: size})
		if err != nil {
			return nil, err
		}
		return map[string]any{"volume": protoJSON(created), "note": stagedNote(false)}, nil
	})

	add(server, readTool("get_volume", "Get one volume by ID: size, usage, pinned node, state."), func(ctx context.Context, in getVolumeIn) (any, error) {
		if err := required("volume_id", in.VolumeID); err != nil {
			return nil, err
		}
		return s.client.GetVolume(ctx, in.VolumeID)
	})

	type updateVolumeIn struct {
		VolumeID  string `json:"volume_id"`
		SizeGib   int64  `json:"size_gib,omitempty"`
		SizeBytes int64  `json:"size_bytes,omitempty"`
	}
	add(server, idempotentWriteTool("update_volume", "Grow a volume. Shrinking is rejected by the platform."), func(ctx context.Context, in updateVolumeIn) (any, error) {
		if err := required("volume_id", in.VolumeID); err != nil {
			return nil, err
		}
		size, err := volumeSize(in.SizeGib, in.SizeBytes)
		if err != nil {
			return nil, err
		}
		return s.client.UpdateVolume(ctx, in.VolumeID, size)
	})

	type deleteVolumeIn struct {
		VolumeID string `json:"volume_id"`
		Confirm  bool   `json:"confirm,omitempty"`
	}
	add(server, destructiveTool("delete_volume", "Delete a volume and, after grace, its data. Destructive: preview first, then confirm:true."), func(ctx context.Context, in deleteVolumeIn) (any, error) {
		if err := required("volume_id", in.VolumeID); err != nil {
			return nil, err
		}
		volume, err := s.findVolume(ctx, in.VolumeID)
		if err != nil {
			return nil, err
		}
		if !in.Confirm {
			detail, err := s.client.PreviewVolumeDeletion(ctx, in.VolumeID)
			if err != nil {
				return nil, err
			}
			name := ""
			if volume != nil {
				name = volume.GetName()
			}
			return preview(fmt.Sprintf("delete volume %q (%s)", name, in.VolumeID), map[string]any{"would_affect": protoJSON(detail)}), nil
		}
		if volume == nil {
			return nil, fmt.Errorf("volume %s not found in its environment listing", in.VolumeID)
		}
		if err := s.client.DeleteVolume(ctx, in.VolumeID, volume.GetName()); err != nil {
			return nil, err
		}
		return deleted("volume", in.VolumeID, volume.GetName()), nil
	})
}

func volumeSize(gib, bytes int64) (int64, error) {
	switch {
	case gib > 0 && bytes > 0:
		return 0, fmt.Errorf("pass size_gib or size_bytes, not both")
	case gib > 0:
		return gib * gibibyte, nil
	case bytes > 0:
		return bytes, nil
	default:
		return 0, fmt.Errorf("size_gib or size_bytes is required")
	}
}

func primaryPort(svc *platformv1.Service) int32 {
	ports := svc.GetSpec().GetRuntime().GetPorts()
	for _, port := range ports {
		if port.GetPrimary() {
			return port.GetPort()
		}
	}
	if len(ports) > 0 {
		return ports[0].GetPort()
	}
	return 0
}

func (s *state) findVolume(ctx context.Context, volumeID string) (*platformv1.Volume, error) {
	return s.client.GetVolume(ctx, volumeID)
}

type getVolumeIn struct {
	VolumeID string `json:"volume_id"`
}
