package productionops

import (
	"context"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/recovery"
	"encoding/json"
	"fmt"
	"time"
)

// captureExternalInventory runs with host credentials, independently of platform
// login. An absent machine retains its last complete inventory, rather than
// removing unknown resources because a heartbeat has expired.
func (r *Runner) captureExternalInventory(ctx context.Context) error {
	var previous recovery.FleetInput
	if err := privateJSON(r.Plan.Installation.Recovery.Inventory, &previous); err != nil {
		// Fresh installations have no prior observation.
		if r.Plan.Previous != nil {
			return fmt.Errorf("latest independent fleet inventory unavailable: %w", err)
		}
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	current := recovery.FleetInput{CapturedAt: time.Now().UTC()}
	current.Desired, err = recovery.ReadDesiredFleet(ctx, db)
	if err != nil {
		return err
	}
	current.DesiredResources, err = recovery.ReadDesiredResources(ctx, db)
	if err != nil {
		return err
	}
	current.DesiredNetworks, err = recovery.ReadDesiredNetworks(ctx, db)
	if err != nil {
		return err
	}
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent {
			continue
		}
		body, err := r.hostProbe(ctx, r.Plan, pl, r.expandProbe(r.Config.Probes[deploy.Agent], pl), false)
		if err != nil {
			found := false
			for _, old := range previous.Observed {
				if old.ID == pl.Instance {
					old.Reachable = false
					current.Observed = append(current.Observed, old)
					found = true
				}
			}
			if !found {
				return fmt.Errorf("agent %s has never produced a complete independent inventory", pl.Instance)
			}
			continue
		}
		var report health.Report
		if err = json.Unmarshal(body, &report); err != nil {
			return err
		}
		var host recovery.FleetHost
		if err = json.Unmarshal(report.Inventory, &host); err != nil {
			return err
		}
		if host.ID != pl.Instance || host.Generation != r.Plan.Generation || report.Authority == nil || report.Authority.Generation != r.Plan.Generation {
			return fmt.Errorf("independent inventory identity differs")
		}
		current.Observed = append(current.Observed, host)
	}
	return saveJSON(r.Plan.Installation.Recovery.Inventory, current)
}
