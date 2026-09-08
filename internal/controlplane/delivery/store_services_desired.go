package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *persistence) placementCandidatesQuerier(ctx context.Context, q ServiceQueryer) ([]placementCandidate, error) {
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

func (s *persistence) requireVolumeQuerier(ctx context.Context, q ServiceQueryer, environmentID, volumeName string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM volumes WHERE environment_id = $1 AND name = $2`, environmentID, volumeName).Scan(&one)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", ErrVolumeNotFound, volumeName)
	default:
		return err
	}
}

func (s *persistence) domainTargetPortsForService(ctx context.Context, serviceID string) ([]int32, error) {
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
