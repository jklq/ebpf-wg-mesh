package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"ebof-wg-mesh/internal/restartpolicy"
)

func (s *Store) placementCandidatesQuerier(ctx context.Context, q serviceQueryer) ([]placementCandidate, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT a.id,
		        a.region,
		        a.zone,
		        a.failure_domain,
		        a.runtime_capabilities,
		        greatest(a.cpu_millis_capacity - a.reserved_cpu_millis, 0),
		        greatest(a.memory_mebibytes_capacity - a.reserved_memory_mebibytes, 0),
		        COALESCE(stats.service_count, 0),
		        COALESCE(stats.cpu_millis, 0),
		        COALESCE(stats.memory_mebibytes, 0)
		   FROM agents a
		   LEFT JOIN (
				SELECT a.agent_id,
		               COUNT(*) AS service_count,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
			  FROM services s
			  JOIN allocations a ON a.service_id = s.id
		          JOIN service_revisions r
		            ON r.service_id = s.id
		           AND r.spec_revision = s.current_spec_revision
			 GROUP BY a.agent_id
		   ) AS stats
		     ON stats.agent_id = a.id
		  WHERE a.last_seen_at > statement_timestamp() - INTERVAL '30 seconds' AND a.lifecycle_state = 'active'
		  ORDER BY COALESCE(stats.service_count, 0) ASC, a.id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []placementCandidate
	for rows.Next() {
		var candidate placementCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.Region,
			&candidate.Zone,
			&candidate.FailureDomain,
			(*jsonStringSlice)(&candidate.RuntimeCapabilities),
			&candidate.CPUMillisCapacity,
			&candidate.MemoryMebibytesCapcity,
			&candidate.ServiceCount,
			&candidate.UsedCPUMillis,
			&candidate.UsedMemoryMebibytes,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) requireVolumeQuerier(ctx context.Context, q serviceQueryer, environmentID, volumeName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM volumes WHERE environment_id = $1 AND name = $2`, environmentID, volumeName).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", errVolumeNotFound, volumeName)
	default:
		return err
	}
}

func (s *Store) domainTargetPortsForService(ctx context.Context, serviceID string) ([]int32, error) {
	rows, err := s.db.QueryContext(ctx,
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

func (s *Store) listHealthyIngressBackends(ctx context.Context) ([]ingressBackend, error) {
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
		allocationRolloutServing,
		restartpolicy.PhaseCrashLoop,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
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
			backends = append(backends, ingressBackend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv4, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		} else if slices.Contains(healthyIPv6Ports, targetPort) && net.ParseIP(allocationIPv6) != nil {
			backends = append(backends, ingressBackend{
				Domain: domain, Upstream: net.JoinHostPort(allocationIPv6, strconv.Itoa(int(targetPort))), AllocationID: allocationID,
			})
		}
	}
	return backends, rows.Err()
}

func equalInt32Slices(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type ingressBackend struct {
	Domain       string
	Upstream     string
	AllocationID string
}
