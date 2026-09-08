package controlplane

import (
	"context"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/restartpolicy"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
)

func (s *routingPersistence) HealthyIngressBackends(ctx context.Context) ([]routing.Backend, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.hostname, d.target_port, a.healthy_ipv4_ports, a.healthy_ipv6_ports,
		        a.allocation_ipv4, a.allocation_ipv6, a.id
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		   JOIN services s ON s.id = a.service_id
		  WHERE a.healthy = TRUE
		    AND a.rollout_state = $1
		    AND a.applied_spec_revision >= a.desired_spec_revision
		    AND a.applied_rollout_generation >= a.desired_rollout_generation
		    AND a.phase <> $2
		  ORDER BY d.hostname ASC, a.id ASC`,
		deliverycore.AllocationRolloutServing,
		restartpolicy.PhaseCrashLoop,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []routing.Backend
	for rows.Next() {
		var (
			domain           string
			targetPort       int32
			healthyIPv4Ports []int32
			healthyIPv6Ports []int32
			allocationIPv4   string
			allocationIPv6   string
			allocationID     string
		)
		if err := rows.Scan(&domain, &targetPort, (*jsonInt32Slice)(&healthyIPv4Ports), (*jsonInt32Slice)(&healthyIPv6Ports), &allocationIPv4, &allocationIPv6, &allocationID); err != nil {
			return nil, err
		}
		if slices.Contains(healthyIPv4Ports, targetPort) && net.ParseIP(allocationIPv4) != nil {
			backends = append(backends, routing.Backend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv4, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		} else if slices.Contains(healthyIPv6Ports, targetPort) && net.ParseIP(allocationIPv6) != nil {
			backends = append(backends, routing.Backend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv6, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		}
	}
	return backends, rows.Err()
}

type jsonInt32Slice []int32

func (p *jsonInt32Slice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]int32)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]int32)(p))
	default:
		return fmt.Errorf("scan int32 slice json: unsupported type %T", src)
	}
}
