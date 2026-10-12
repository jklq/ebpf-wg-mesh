package productionops

import (
	"context"
	"database/sql"
	"fmt"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/deploy"
)

// overlapReservation is the agent capacity held back so a host can run one
// overlapping process generation during an upgrade.
func overlapReservation(p deploy.Plan, pl deploy.Placement) deploy.Resources {
	h, _ := p.Previous.Installation.Host(pl.Host)
	old := h.Reserve
	for _, previous := range p.Previous.Placements {
		if previous.Host == pl.Host {
			old = old.Add(deploy.ComponentResources(p.Previous.Installation, p.Previous.Release, previous.Role))
		}
	}
	peak := p.Upgrade.Peak[pl.Host]
	return deploy.Resources{CPUMillis: max(peak.CPUMillis-old.CPUMillis, 0), MemoryMiB: max(peak.MemoryMiB-old.MemoryMiB, 0)}
}

// This changes only admission capacity. It does not cordon, relocate or kill
// allocations to manufacture upgrade headroom on a resource-constrained host.
func (r *Runner) reserveOverlap(ctx context.Context, db *sql.DB, pl deploy.Placement, reserve deploy.Resources) error {
	return adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		var cpu, ram int64
		var reservedCPU, reservedRAM int64
		if err := tx.QueryRowContext(ctx, `SELECT cpu_millis_capacity,memory_mebibytes_capacity,reserved_cpu_millis,reserved_memory_mebibytes FROM agent_registrations WHERE id=$1 FOR UPDATE`, pl.Instance).Scan(&cpu, &ram, &reservedCPU, &reservedRAM); err != nil {
			return err
		}
		reserve.CPUMillis = max(reserve.CPUMillis, reservedCPU)
		reserve.MemoryMiB = max(reserve.MemoryMiB, reservedRAM)
		var usedCPU, usedRAM int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8,0)),0),COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8,0)),0) FROM allocation_assignments a JOIN service_revisions r ON r.service_id=a.service_id AND r.spec_revision=a.desired_spec_revision WHERE a.agent_id=$1 AND a.rollout_state<>'lost'`, pl.Instance).Scan(&usedCPU, &usedRAM); err != nil {
			return err
		}
		if cpu <= 0 || ram <= 0 || usedCPU+reserve.CPUMillis > cpu || usedRAM+reserve.MemoryMiB > ram {
			return fmt.Errorf("existing allocations leave insufficient transition headroom; serving workloads retained")
		}
		_, err := journal.AgentRow(pl.Instance).Exec(ctx, tx, `UPDATE agent_registrations SET reserved_cpu_millis=$2,reserved_memory_mebibytes=$3,updated_at=statement_timestamp() WHERE id=$1`, pl.Instance, reserve.CPUMillis, reserve.MemoryMiB)
		return err
	})
}

func (r *Runner) releaseOverlapReservations(ctx context.Context) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	return adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		for _, pl := range r.Plan.Placements {
			if pl.Role == deploy.Agent {
				if _, err := journal.AgentRow(pl.Instance).Exec(ctx, tx, `UPDATE agent_registrations SET reserved_cpu_millis=0,reserved_memory_mebibytes=0,updated_at=statement_timestamp() WHERE id=$1`, pl.Instance); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
