package productionops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/deploy"
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
	// Native reservation is applied in the agent cgroups and advertised capacity.
	// Scheduler rows reserve only additional capacity, avoiding double subtraction.
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent {
			continue
		}
		h, _ := r.Plan.Installation.Host(pl.Host)
		var cpu, ram int64
		if err = db.QueryRowContext(ctx, `SELECT cpu_millis_capacity,memory_mebibytes_capacity FROM agent_registrations WHERE id=$1`, pl.Instance).Scan(&cpu, &ram); err != nil {
			return err
		}
		reserve := r.Plan.Reservations[pl.Host]
		if cpu > h.Capacity.CPUMillis-reserve.CPUMillis || ram > h.Capacity.MemoryMiB-reserve.MemoryMiB || cpu <= 0 || ram <= 0 {
			return fmt.Errorf("agent has not advertised its native applied reservation")
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
