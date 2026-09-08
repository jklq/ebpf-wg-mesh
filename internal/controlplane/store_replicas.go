package controlplane

import (
	"context"
	"strings"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

func countReadyAllocations(recs []deliverycore.AllocationRecord) int32 {
	var ready int32
	for _, rec := range recs {
		if deliverycore.AllocationReady(rec) {
			ready++
		}
	}
	return ready
}

func (s *readsPersistence) agentIDsForServiceQuerier(ctx context.Context, q deliverycore.ServiceQueryer, serviceID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT agent_id FROM allocations WHERE service_id = $1 ORDER BY agent_id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func allocationRestartObserved(prevPhase string, prevApplied int64, nextPhase string, nextApplied int64) bool {
	if prevApplied > 0 && nextApplied == 0 {
		return true
	}
	return allocationWasServing(prevPhase) && allocationIsStarting(nextPhase)
}

func allocationWasServing(phase string) bool {
	switch strings.TrimSpace(phase) {
	case "Running", "Healthy":
		return true
	default:
		return false
	}
}

func allocationIsStarting(phase string) bool {
	switch strings.TrimSpace(phase) {
	case "Pending", "Starting":
		return true
	default:
		return false
	}
}
