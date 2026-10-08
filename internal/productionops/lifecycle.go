package productionops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/ingressnodes"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/reconciliation"
)

func systemctl(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "systemctl", args...).Run()
}
func mutable(role deploy.Role) bool {
	return role == deploy.ControlPlane || role == deploy.Console || role == deploy.Builder || role == deploy.Agent
}

func (r *Runner) pause(ctx context.Context, paused, checkpoints bool) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE recovery_runtime_authority SET paused=$1 WHERE singleton=TRUE AND installation=$2 AND generation=$3`, paused, r.Plan.Installation.ID, r.Plan.Generation)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("runtime authority was not admitted for this installation and generation")
	}
	if _, err := db.ExecContext(ctx, `UPDATE build_scheduler_control SET paused=$1,updated_at=statement_timestamp() WHERE id=TRUE`, paused); err != nil {
		return err
	}
	return r.installAdmission(ctx, paused, checkpoints, true)
}
func (r *Runner) installAdmission(ctx context.Context, paused, checkpoints, restart bool) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		return err
	}
	defer keys.Close()
	ca, err := signing.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return err
	}
	admitted := reconciliation.Authority{InstallationID: r.Plan.Installation.ID, Generation: r.Plan.Generation, ClusterID: clusterID(ca), Paused: paused, Checkpoints: checkpoints}
	b, _ := json.Marshal(admitted)
	for _, p := range r.participantPlans(paused) {
		for _, pl := range p.Placements {
			if !mutable(pl.Role) {
				continue
			}
			script := remoteFile(cfgDir(p, pl)+"/authority.json", b)
			if restart {
				script += "if systemctl cat " + shell(unit(p, pl)) + " >/dev/null 2>&1; then systemctl restart " + shell(unit(p, pl)) + "; fi\n"
			}
			if _, err := r.remote(ctx, p, pl, script); err != nil {
				// An offline retained participant is safe only with independently
				// verified provider/host fencing; heartbeat expiry never suffices.
				if !r.Plan.Recovery || r.verifyHostFenced(ctx, p, pl.Host) != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (r *Runner) participantPlans(paused bool) []deploy.Plan {
	if paused && !r.Plan.Recovery && r.Plan.Previous != nil {
		old := r.Plan.Previous
		return []deploy.Plan{{Installation: old.Installation, Release: old.Release, Placements: old.Placements, Previous: old, Generation: r.Plan.Generation}}
	}
	return []deploy.Plan{r.Plan}
}
func (r *Runner) verifyPause(ctx context.Context, paused, recovered bool) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var observed bool
	var installation, generation string
	if err := db.QueryRowContext(ctx, `SELECT installation,generation,paused FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&installation, &generation, &observed); err != nil {
		return err
	}
	if observed != paused || installation != r.Plan.Installation.ID || generation != r.Plan.Generation {
		return fmt.Errorf("database authority differs from required pause/generation")
	}
	for _, p := range r.participantPlans(paused) {
		for _, pl := range p.Placements {
			if !mutable(pl.Role) {
				continue
			}
			if r.Plan.Automatic && (pl.Role == deploy.Agent || pl.Role == deploy.Builder) {
				continue
			}
			out, err := r.remote(ctx, p, pl, "systemctl is-active --quiet "+shell(unit(p, pl))+"\ncat "+shell(cfgDir(p, pl)+"/authority.json")+"\n")
			if err != nil {
				return err
			}
			var a reconciliation.Authority
			if err := json.Unmarshal(out, &a); err != nil {
				return err
			}
			if a.Paused != paused || a.Generation != generation || a.InstallationID != installation {
				return fmt.Errorf("host authority does not match database")
			}
			if pl.Role != deploy.Console {
				probe := r.expandProbe(r.Config.Probes[pl.Role], pl)
				// The process reports the authority it admitted at startup. A changed
				// file without a restart cannot satisfy this observation.
				body, err := r.hostProbe(ctx, p, pl, probe, false)
				if err != nil {
					return err
				}
				var report health.Report
				if err := json.Unmarshal(body, &report); err != nil {
					return err
				}
				if report.Authority == nil || report.Authority.Paused != paused || report.Authority.Generation != generation || report.Authority.InstallationID != installation || report.Authority.ClusterID != a.ClusterID {
					return fmt.Errorf("running %s has not admitted the selected authority", pl.Instance)
				}
			} else {
				// Console readiness includes the admitted runtime generation and pause.
				body, err := r.hostProbe(ctx, p, pl, r.expandProbe(r.Config.Probes[pl.Role], pl), false)
				if err != nil {
					return err
				}
				var report struct {
					Authority *reconciliation.Authority `json:"authority"`
				}
				if err := json.Unmarshal(body, &report); err != nil {
					return err
				}
				if report.Authority == nil || report.Authority.Generation != generation || report.Authority.Paused != paused {
					return fmt.Errorf("console has not admitted the selected authority")
				}
			}
		}
	}
	if recovered && !r.Plan.Recovery {
		return fmt.Errorf("recovery resume cannot activate a normal deployment")
	}
	return nil
}
func (r *Runner) resume(ctx context.Context, recovered bool) (returnErr error) {
	defer func() {
		if returnErr != nil {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if err := r.pause(pauseCtx, true, recovered); err != nil {
				returnErr = fmt.Errorf("%w; pause unresolved: %v", returnErr, err)
			}
		}
	}()
	if err := r.productionVerify(ctx); err != nil {
		return fmt.Errorf("resume gate: %w", err)
	}
	if recovered {
		if err := r.recoverWork(ctx, true); err != nil {
			return err
		}
		if err := r.admitBuilders(ctx); err != nil {
			return err
		}
		if r.RecoveryProgress == nil {
			return fmt.Errorf("resume needs recorded recovery progress")
		}
		if err := r.writeNativeInputs(); err != nil {
			return err
		}
		var out strings.Builder
		if err := deploy.RunRecovery(ctx, []string{"resume-authority", "--database-url-file", r.Config.Database.URLFile, "--installation", r.Plan.Installation.ID, "--generation", r.Plan.Generation, "--plan", r.planFile(), "--progress", r.progressFile()}, &out); err != nil {
			return err
		}
	}
	if err := r.pause(ctx, false, false); err != nil {
		return err
	}
	return r.verifyPause(ctx, false, recovered)
}
func (r *Runner) lifecycle(ctx context.Context, args []string, verify bool) error {
	if len(args) != 3 {
		return fmt.Errorf("lifecycle requires role and instance")
	}
	action := args[0]
	if action == "verify-drain" {
		action = "drain"
	}
	if action == "verify-retirement" {
		action = "retire"
	}
	pl, p, err := r.findPlacement(deploy.Role(args[1]), args[2])
	if err != nil {
		return err
	}
	if pl.Role == deploy.Database {
		return r.databaseRetirement(ctx, p, pl, action, verify)
	}
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if action == "drain" {
		if pl.Role == deploy.Agent {
			if !verify {
				if err := drainAgent(ctx, db, pl.Instance); err != nil {
					return err
				}
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM allocation_assignments WHERE agent_id=$1`, pl.Instance).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("agent owns allocations; explicit workload relocation is required before retirement")
			}
		}
		if pl.Role == deploy.Builder {
			if !verify {
				if _, err := db.ExecContext(ctx, `UPDATE builder_workers SET drained=TRUE,updated_at=statement_timestamp() WHERE id=$1`, pl.Instance); err != nil {
					return err
				}
			}
			var active int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM build_runs WHERE state='running' AND builder_id=$1`, pl.Instance).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return fmt.Errorf("builder still owns an active build")
			}
		}
		// Stopping an ingress closes public traffic before permanent membership
		// retirement. Existing workload containers remain owned by their agent.
		if !verify {
			_, err = r.remote(ctx, p, pl, "systemctl disable --now "+shell(unit(p, pl))+"\n")
			return err
		}
	} else if !verify {
		if _, err := r.remote(ctx, p, pl, "! systemctl is-active --quiet "+shell(unit(p, pl))+"\n"); err != nil {
			return err
		}
		if pl.Role == deploy.Envoy {
			if err := ingressnodes.New(db).Retire(ctx, pl.Instance); err != nil {
				return err
			}
		}
		if pl.Role == deploy.Agent {
			if err := r.retireAgent(ctx, db, pl.Instance); err != nil {
				return err
			}
		}
		_, err = r.remote(ctx, p, pl, maskScript(p, pl))
		return err
	}
	_, err = r.remote(ctx, p, pl, "! systemctl is-active --quiet "+shell(unit(p, pl))+"\n")
	if err != nil {
		return err
	}
	if action == "retire" && pl.Role == deploy.Envoy {
		active, err := ingressnodes.New(db).NodeActive(ctx, pl.Instance)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("ingress remains an active member")
		}
	}
	if action == "retire" {
		if _, err := r.remote(ctx, p, pl, "test \"$(readlink "+shell("/etc/systemd/system/"+unit(p, pl))+")\" = /dev/null\n"); err != nil {
			return err
		}
		if pl.Role == deploy.Agent {
			var retired bool
			if err := db.QueryRowContext(ctx, `SELECT lifecycle_state='retired' AND operator_intent='cordoned' AND credential_revoked_at IS NOT NULL FROM agent_administration WHERE agent_id=$1`, pl.Instance).Scan(&retired); err != nil {
				return err
			}
			if !retired {
				return fmt.Errorf("agent retirement and credential revocation are unresolved")
			}
		}
		if pl.Role == deploy.Builder {
			var drained bool
			if err := db.QueryRowContext(ctx, `SELECT drained AND current_build_id='' FROM builder_workers WHERE id=$1`, pl.Instance).Scan(&drained); err != nil {
				return err
			}
			if !drained {
				return fmt.Errorf("builder retirement leaves active ownership")
			}
		}
	}

	return nil
}
func (r *Runner) findPlacement(role deploy.Role, id string) (deploy.Placement, deploy.Plan, error) {
	for _, pl := range r.Plan.Placements {
		if pl.Role == role && pl.Instance == id {
			return pl, r.Plan, nil
		}
	}
	if r.Plan.Previous != nil {
		for _, pl := range r.Plan.Previous.Placements {
			if pl.Role == role && pl.Instance == id {
				return pl, deploy.Plan{Installation: r.Plan.Previous.Installation, Release: r.Plan.Previous.Release, Placements: r.Plan.Previous.Placements}, nil
			}
		}
	}
	return deploy.Placement{}, deploy.Plan{}, fmt.Errorf("lifecycle target is absent from the applied inventory")
}
func (r *Runner) databaseRetirement(ctx context.Context, p deploy.Plan, pl deploy.Placement, action string, verify bool) error {
	h, _ := p.Installation.Host(pl.Host)
	// Resolve the native node ID from its actual address, never its ordinal.
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	identityPath := filepath.Join(r.Config.StateDirectory, r.Plan.ID, "retired-node-"+pl.Instance+".json")
	var identity struct {
		Node    int64
		Address string
	}
	id, resolveErr := databaseNodeID(ctx, db, h.Network.Address)
	if err := privateJSON(identityPath, &identity); err == nil {
		if identity.Address != h.Network.Address || identity.Node <= 0 || (resolveErr == nil && id != identity.Node) {
			return fmt.Errorf("retained native node identity differs")
		}
		if resolveErr != nil && resolveErr != sql.ErrNoRows {
			return resolveErr
		}
		id = identity.Node
	} else {
		if !os.IsNotExist(err) {
			return err
		}
		if resolveErr != nil {
			return resolveErr
		}
		identity.Node, identity.Address = id, h.Network.Address
		if err := saveJSON(identityPath, identity); err != nil {
			return err
		}
	}
	if !verify {
		cmd := exec.CommandContext(ctx, r.Config.Database.Binary, "node", "decommission", fmt.Sprint(id), "--wait=all", "--host="+r.reachableDatabase(ctx), "--certs-dir="+r.databasePKI())
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("native database decommission incomplete: %w", err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET allow_unsafe_internals = true`); err != nil {
		return err
	}
	var replicas int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM crdb_internal.ranges WHERE $1=ANY(replicas)`, id).Scan(&replicas); err != nil {
		return err
	}
	if replicas != 0 {
		return fmt.Errorf("database node still holds replicas")
	}
	var membership string
	if err := conn.QueryRowContext(ctx, `SELECT membership FROM crdb_internal.kv_node_liveness WHERE node_id=$1`, id).Scan(&membership); err != nil {
		return err
	}
	if membership != "decommissioned" {
		return fmt.Errorf("native database membership retirement is unresolved")
	}
	if action == "retire" && !verify {
		_, err = r.remote(ctx, p, pl, maskScript(p, pl))
		return err
	}
	if action == "retire" {
		_, err = r.remote(ctx, p, pl, "! systemctl is-active --quiet "+shell(unit(p, pl))+"\ntest \"$(readlink "+shell("/etc/systemd/system/"+unit(p, pl))+")\" = /dev/null\n")
		return err
	}
	return nil
}
