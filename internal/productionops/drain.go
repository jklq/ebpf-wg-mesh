package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/recovery"
)

func drainAgent(ctx context.Context, db *sql.DB, id string) error {
	if err := adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := journal.AdministrationRow(id).Exec(ctx, tx, `UPDATE agent_administration SET lifecycle_state='draining',operator_intent='draining',maintenance_message='platform plan requested safe stateless drain',updated_at=statement_timestamp() WHERE agent_id=$1 AND lifecycle_state<>'retired'`, id)
		return err
	}); err != nil {
		return err
	}
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var remaining int
		var message string
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM allocation_assignments WHERE agent_id=$1 AND rollout_state<>'lost'`, id).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return nil
		}
		if err := db.QueryRowContext(ctx, `SELECT maintenance_message FROM agent_administration WHERE agent_id=$1`, id).Scan(&message); err != nil {
			return err
		}
		if strings.HasPrefix(message, "drain paused") {
			return fmt.Errorf("native safe drain is blocked: %s", message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("native drain pending; host and workload contents preserved")
		case <-ticker.C:
		}
	}
}
func (r *Runner) yieldReservations(ctx context.Context, db *sql.DB, pl deploy.Placement) error {
	if r.Plan.Previous == nil || r.Plan.Recovery {
		return nil
	}
	oldHost, ok := r.Plan.Previous.Installation.Host(pl.Host)
	if !ok {
		return nil
	}
	old := oldHost.Reserve
	for _, previous := range r.Plan.Previous.Placements {
		if previous.Host == pl.Host {
			old = old.Add(r.Plan.Previous.Installation.Components[previous.Role].Resources)
		}
	}
	next := r.Plan.Reservations[pl.Host]
	if old.CPUMillis >= next.CPUMillis && old.MemoryMiB >= next.MemoryMiB {
		return nil
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_recovery.public.reservation_admission(plan_id STRING,agent_id STRING,previous_state STRING,previous_intent STRING,PRIMARY KEY(plan_id,agent_id))`); err != nil {
		return err
	}
	if err := adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform_recovery.public.reservation_admission SELECT $1,agent_id,lifecycle_state,operator_intent FROM agent_administration WHERE agent_id=$2 ON CONFLICT DO NOTHING`, r.Plan.ID, pl.Instance)
		return err
	}); err != nil {
		return err
	}
	return drainAgent(ctx, db, pl.Instance)
}
func (r *Runner) admitReservations(ctx context.Context) error {
	if r.Plan.Previous == nil || r.Plan.Recovery {
		return nil
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var paused bool
	if err := db.QueryRowContext(ctx, `SELECT paused FROM recovery_runtime_authority WHERE singleton=TRUE AND installation=$1 AND generation=$2`, r.Plan.Installation.ID, r.Plan.Generation).Scan(&paused); err != nil {
		return err
	}
	// Native reservation is applied in the agent cgroups and advertised capacity.
	// Scheduler rows reserve only additional capacity, avoiding double subtraction.
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent || !r.managedCredentials(pl) {
			continue
		}
		h, _ := r.Plan.Installation.Host(pl.Host)
		probe := r.expandProbe(r.Config.Probes[deploy.Agent], pl)
		if probe.RuntimeURL != "" {
			probe.URL = probe.RuntimeURL
		}
		body, err := r.hostProbe(ctx, r.Plan, pl, probe, false)
		if err != nil {
			return err
		}
		var live health.Report
		if err := json.Unmarshal(body, &live); err != nil {
			return err
		}
		var inventory recovery.FleetHost
		if err := json.Unmarshal(live.Inventory, &inventory); err != nil {
			return err
		}
		if live.Capacity == nil || live.Authority == nil || live.Authority.InstallationID != r.Plan.Installation.ID || live.Authority.Generation != r.Plan.Generation || live.Authority.Paused != paused || inventory.ID != pl.Instance || inventory.Generation != r.Plan.Generation || inventory.LocalStoreID == "" {
			return fmt.Errorf("agent %s has not reported its live admitted capacity and local identity", pl.Instance)
		}
		cpu, ram := live.Capacity.CPUMillis, live.Capacity.MemoryMiB
		reserve := r.Plan.Reservations[pl.Host]
		if cpu > h.Capacity.CPUMillis-reserve.CPUMillis || ram > h.Capacity.MemoryMiB-reserve.MemoryMiB || cpu <= 0 || ram <= 0 {
			return fmt.Errorf("agent has not advertised its native applied reservation")
		}
		// Inspect the live kernel limits, not just the agent's configuration.
		script := "pid=$(systemctl show --value -p MainPID " + shell(unit(r.Plan, pl)) + ")\ntest \"$pid\" -gt 0\n"
		script += "cg=$(sed -n 's/^0:://p' /proc/$pid/cgroup)\ntest -n \"$cg\"\n"
		script += fmt.Sprintf("test \"$(cat /sys/fs/cgroup\"$cg\"/memory.min)\" -ge %d\ntest \"$(cat /sys/fs/cgroup\"$cg\"/memory.low)\" -ge %d\n", reserve.MemoryMiB*1024*1024, reserve.MemoryMiB*1024*1024)
		script += "test \"$(cat /sys/fs/cgroup\"$cg\"/cpu.weight)\" = 10000\n"
		script += fmt.Sprintf("test \"$(cat /sys/fs/cgroup/ebpf-wg-mesh-workloads/memory.max)\" = %d\n", ram*1024*1024)
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return fmt.Errorf("agent %s native reservation differs from its live resource budget: %w", pl.Instance, err)
		}
		// Paused Sync collects inventory without registering a session. Admit only
		// the observed budget for this existing local identity; do not allocate
		// networks, admit checkpoints, or enable workload reconciliation. Restores
		// use approved fleet reconciliation instead of this upgrade-only path.
		if err := adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
			result, err := journal.AgentRow(pl.Instance).Exec(ctx, tx, `UPDATE agent_registrations SET cpu_millis_capacity=$2,memory_mebibytes_capacity=$3,local_store_id=$4 FROM agent_administration a WHERE agent_registrations.id=$1 AND a.agent_id=agent_registrations.id AND a.lifecycle_state<>'retired' AND a.credential_revoked_at IS NULL AND (local_store_id=$4 OR (local_store_id='' AND a.lifecycle_state='enrolling'))`, pl.Instance, cpu, ram, inventory.LocalStoreID)
			if err != nil {
				return err
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				return fmt.Errorf("agent %s local identity differs from its enrolled registration", pl.Instance)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	var exists bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM [SHOW TABLES FROM platform_recovery] WHERE table_name='reservation_admission')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		for _, pl := range r.Plan.Placements {
			if pl.Role != deploy.Agent {
				continue
			}
			if _, err = journal.AdministrationRow(pl.Instance).Exec(ctx, tx, `UPDATE agent_administration SET lifecycle_state=q.previous_state,operator_intent=q.previous_intent,maintenance_message='',updated_at=statement_timestamp() FROM platform_recovery.public.reservation_admission q WHERE agent_administration.agent_id=q.agent_id AND q.plan_id=$1 AND q.agent_id=$2 AND agent_administration.lifecycle_state='draining'`, r.Plan.ID, pl.Instance); err != nil {
				return err
			}
		}
		return nil
	})
}
