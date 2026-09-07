package delivery

import (
	"context"
)

func (s *persistence) listDomainBindings(ctx context.Context, userID, serviceID string) ([]DomainBindingRecord, error) {
	service, err := s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT hostname, service_id, target_port, platform_generated, created_at, updated_at
		FROM domain_bindings WHERE service_id = $1 ORDER BY hostname ASC`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DomainBindingRecord
	for rows.Next() {
		var binding DomainBindingRecord
		binding.ProjectID = service.ProjectID
		binding.EnvironmentID = service.EnvironmentID
		if err := rows.Scan(&binding.Hostname, &binding.ServiceID, &binding.TargetPort, &binding.PlatformGenerated, &binding.CreatedAt, &binding.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *persistence) serviceStatus(ctx context.Context, userID, serviceID string) (ServiceRecord, []AllocationRecord, error) {
	service, err := s.serviceByID(ctx, userID, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	allocs, err := s.listAllocationsByServiceID(ctx, serviceID)
	if err != nil {
		return ServiceRecord{}, nil, err
	}
	return service, allocs, nil
}
