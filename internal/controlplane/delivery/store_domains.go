package delivery

import (
	"context"

	"ebof-wg-mesh/internal/controlplane/authz"
)

func (s *persistence) listDomainBindings(ctx context.Context, scope authz.Service, includeDeleted bool) ([]DomainBindingRecord, error) {
	filter := `
		    AND d.deleted_at IS NULL AND s.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL`
	if includeDeleted {
		filter = ``
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.hostname, d.service_id, d.target_port, d.platform_generated, d.created_at, d.updated_at,
		d.deleted_at, d.deleted_by_user_id, d.delete_expires_at,
		s.deleted_at, s.deleted_by_user_id, s.delete_expires_at,
		e.deleted_at, e.deleted_by_user_id, e.delete_expires_at,
		p.deleted_at, p.deleted_by_user_id, p.delete_expires_at
		FROM domain_bindings d
		JOIN services s ON s.id = d.service_id
		JOIN environments e ON e.id = s.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE d.service_id = $1`+filter+`
		ORDER BY d.hostname ASC`, scope.ID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DomainBindingRecord
	for rows.Next() {
		var binding DomainBindingRecord
		var self, service, environment, project Tombstone
		binding.ProjectID = scope.ProjectID()
		binding.EnvironmentID = scope.EnvironmentID()
		targets := []any{&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt}
		targets = ScanTombstone(targets, &self)
		targets = ScanTombstone(targets, &service)
		targets = ScanTombstone(targets, &environment)
		if err := rows.Scan(ScanTombstone(targets, &project)...); err != nil {
			return nil, err
		}
		binding.Deletion = EffectiveDeletion(self, service, environment, project)
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *persistence) domainTargetPortsForService(ctx context.Context, serviceID string) ([]int32, error) {
	return domainTargetPortsForServiceQuerier(ctx, s.db, serviceID)
}

func domainTargetPortsForServiceQuerier(ctx context.Context, q ServiceQueryer, serviceID string) ([]int32, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT target_port
		   FROM domain_bindings
		  WHERE service_id = $1
		  ORDER BY hostname ASC`,
		serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ports []int32
	for rows.Next() {
		var port int32
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, rows.Err()
}

func (s *persistence) serviceStatus(ctx context.Context, scope authz.Service) (ServiceRecord, []AllocationRecord, error) {
	service, err := s.serviceByID(ctx, scope)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocs, err := s.listAllocationsByServiceID(ctx, scope.ID())
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return service, allocs, nil
}
