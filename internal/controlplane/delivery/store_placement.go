package delivery

import (
	"context"
)

type placementCandidate struct {
	ID                     string
	Region                 string
	Zone                   string
	FailureDomain          string
	RuntimeCapabilities    []string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func (s *persistence) placementCandidatesQuerier(ctx context.Context, q ServiceQueryer) ([]placementCandidate, error) {
	// New placement requires both active administrative intent and a currently
	// admitted live session. Existing allocations use their own observation and
	// liveness policy and are not withdrawn merely because placement closes.
	rows, err := q.QueryContext(ctx,
		`SELECT r.id,
		        r.region,
		        r.zone,
		        r.failure_domain,
		        r.runtime_capabilities,
		        greatest(r.cpu_millis_capacity - r.reserved_cpu_millis, 0),
		        greatest(r.memory_mebibytes_capacity - r.reserved_memory_mebibytes, 0),
		        COALESCE(stats.service_count, 0),
		        COALESCE(stats.cpu_millis, 0),
		        COALESCE(stats.memory_mebibytes, 0)
		   FROM agent_registrations r
		   JOIN agent_administration ad ON ad.agent_id = r.id
		   LEFT JOIN (
				SELECT a.agent_id,
		               COUNT(*) AS service_count,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
			  FROM services s
			  JOIN allocation_assignments a ON a.service_id = s.id
		          JOIN service_revisions r
		            ON r.service_id = s.id
		           AND r.spec_revision = s.current_spec_revision
			 GROUP BY a.agent_id
		   ) AS stats
		     ON stats.agent_id = r.id
		  WHERE ad.lifecycle_state = 'active'
		  ORDER BY COALESCE(stats.service_count, 0) ASC, r.id ASC`,
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
		if s.live == nil || !s.live.Admitted(candidate.ID) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}
