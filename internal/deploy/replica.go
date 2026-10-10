package deploy

import (
	"context"
	"fmt"
)

// Readiness is checked just before replacing each process, so a lost earlier
// peer cannot be hidden by its durable installation receipt on a retry.
func (d *SSHDriver) verifyReplicaPeers(ctx context.Context, p Plan, state State, target Placement) error {
	seen := map[string]bool{}
	retired := map[Placement]bool{}
	if state.Progress != nil {
		for _, op := range p.Operations {
			if op.Placement != nil && (op.Kind == "stop" || op.Kind == "retire") {
				if _, done := state.Progress.Completed[op.ID]; done {
					retired[*op.Placement] = true
				}
			}
		}
	}
	for _, pl := range append(append([]Placement{}, p.Previous.Placements...), p.Placements...) {
		if pl.Role != target.Role || pl.Instance == target.Instance || retired[pl] || seen[pl.Instance] {
			continue
		}
		seen[pl.Instance] = true
		installed := false
		if state.Progress != nil {
			for _, op := range p.Operations {
				if (op.Kind == "install" || op.Kind == "database-upgrade" || op.Kind == "database-join") && op.Placement != nil && *op.Placement == pl {
					_, installed = state.Progress.Completed[op.ID]
				}
			}
		}
		// A new replica that has not yet started cannot be a surviving peer.
		if !p.previouslyPlaced(pl) && !installed {
			continue
		}
		h, _, found, err := d.discover(ctx, p.Installation, state, pl.Host)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("replica peer %s is unavailable", pl.Instance)
		}
		peerPlan := p
		if !installed {
			peerPlan.Release = p.Previous.Release
		}
		if _, err := d.Remote.Run(ctx, p.Installation, h, command(expandCommand(peerPlan, pl, peerPlan.Release.Programs[pl.Role].Ready))+"\n"); err != nil {
			return fmt.Errorf("replica %s waits for healthy peer %s: %w", target.Instance, pl.Instance, err)
		}
	}
	return nil
}
