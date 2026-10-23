package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/billing"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/restartpolicy"
	"ebof-wg-mesh/internal/telemetry"
)

// Serverless sleep states, stored in service_serverless_state.state. The row
// only exists for services whose applied spec enables serverless; disabling
// serverless wakes the service and deletes the row. See docs/serverless.md.
const (
	ServerlessAwake  = "awake"
	ServerlessAsleep = "asleep"
	ServerlessWaking = "waking"
)

// Wake triggers, used as bounded telemetry labels.
const (
	WakeTriggerPublic   = "public"
	WakeTriggerPrivate  = "private"
	WakeTriggerOperator = "operator"
	WakeTriggerSystem   = "system"
)

const (
	reasonServerlessSleep = "SERVERLESS_SLEEP"
	reasonServerlessWake  = "SERVERLESS_WAKE"
)

// defaultSleepDrainGrace bounds graceful shutdown on sleep when the service
// does not configure a rolling draining time.
const defaultSleepDrainGrace = 10 * time.Second

const maxSleepDrainGrace = 5 * time.Minute

type serverlessRecord struct {
	State        string
	AsleepSince  sql.NullTime
	LastActivity sql.NullTime
	Found        bool
}

func (s *persistence) serverlessRecordTx(ctx context.Context, q ServiceQueryer, serviceID string, forUpdate bool) (serverlessRecord, error) {
	query := `SELECT state, asleep_since, last_activity_at FROM service_serverless_state WHERE service_id = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	rec := serverlessRecord{State: ServerlessAwake}
	err := q.QueryRowContext(ctx, query, serviceID).Scan(&rec.State, &rec.AsleepSince, &rec.LastActivity)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, nil
	}
	if err != nil {
		return serverlessRecord{}, err
	}
	rec.Found = true
	return rec, nil
}

func (s *persistence) upsertServerlessRecordTx(ctx context.Context, tx *sql.Tx, serviceID string, rec serverlessRecord, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO service_serverless_state(service_id, state, asleep_since, last_activity_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (service_id) DO UPDATE SET state = EXCLUDED.state,
			asleep_since = EXCLUDED.asleep_since, last_activity_at = EXCLUDED.last_activity_at, updated_at = EXCLUDED.updated_at`,
		serviceID, rec.State, rec.AsleepSince, rec.LastActivity, now)
	return err
}

func (s *persistence) deleteServerlessRecordTx(ctx context.Context, tx *sql.Tx, serviceID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM service_serverless_state WHERE service_id = $1`, serviceID)
	return err
}

func sleepDrainGrace(service ServiceRecord) time.Duration {
	grace := time.Duration(canonicalRollingStrategy(service.Spec.GetRollingStrategy()).GetDrainingSeconds()) * time.Second
	if grace <= 0 {
		return defaultSleepDrainGrace
	}
	if grace > maxSleepDrainGrace {
		return maxSleepDrainGrace
	}
	return grace
}

// recordServerlessTransitionTx appends a same-state deployment-history entry
// so sleep and wake are visible next to the deployment they belong to. It is
// best-effort in the sense that a service without a current deployment
// simply has nothing to annotate.
func (s *persistence) recordServerlessTransitionTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, reasonCode, detail string, now time.Time) error {
	var deploymentID, state, artifactID string
	err := tx.QueryRowContext(ctx, `SELECT id, state, COALESCE(artifact_id, '') FROM deployments
		WHERE service_id = $1 AND is_current ORDER BY created_at DESC, id DESC LIMIT 1`, service.ID).Scan(&deploymentID, &state, &artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.insertDeploymentTransitionTx(ctx, tx, deploymentID, state, state,
		DeploymentCauseSystem, "", reasonCode, sanitizeDeploymentDetail(detail),
		service.SpecRevision, artifactID, service.RolloutGeneration, now)
}

// sleepServiceTx stops every live allocation of the service while retaining
// its assignments (and therefore addresses, identity seeds, and host
// entries) for a later wake. Callers hold schedulerMu and own ingress
// republication after commit.
func (d *Delivery) sleepServiceTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, now time.Time) (agentIDs []string, err error) {
	s := d.store
	rec, err := s.serverlessRecordTx(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	if rec.State == ServerlessAsleep {
		return nil, nil
	}
	allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	deadline := now.Add(sleepDrainGrace(service))
	var mutations []allocationMutation
	seen := make(map[string]struct{})
	for _, alloc := range allocations {
		if alloc.RolloutState == AllocationRolloutLost || alloc.RolloutState == AllocationRolloutSleeping {
			continue
		}
		mutations = append(mutations, allocationMutation{Kind: mutationChangeIntent, State: allocationAssignmentState{
			AllocationID:  alloc.ID,
			RolloutState:  AllocationRolloutSleeping,
			Intent:        allocationIntentSleep,
			IntentMessage: "sleeping (serverless idle)",
			DrainStarted:  sql.NullTime{Time: now, Valid: true},
			DrainDeadline: sql.NullTime{Time: deadline, Valid: true},
			UpdatedAt:     now,
		}})
		if _, ok := seen[alloc.AgentID]; !ok {
			seen[alloc.AgentID] = struct{}{}
			agentIDs = append(agentIDs, alloc.AgentID)
		}
	}
	if err := d.applyAllocationMutationsTx(ctx, tx, now, mutations...); err != nil {
		return nil, err
	}
	rec.State = ServerlessAsleep
	rec.AsleepSince = sql.NullTime{Time: now, Valid: true}
	if err := s.upsertServerlessRecordTx(ctx, tx, service.ID, rec, now); err != nil {
		return nil, err
	}
	if err := s.recordServerlessTransitionTx(ctx, tx, service, reasonServerlessSleep, "service slept after inactivity (serverless)", now); err != nil {
		return nil, err
	}
	return agentIDs, nil
}

// wakeSleepingAssignmentsTx flips sleeping assignments back to run without
// touching replica counts. Sleeping allocations were serving when they
// slept, so wake restores serving: health gating keeps them out of ingress
// and host entries until the agent reports them healthy again, exactly
// like a container restart. (Rollout creation wakes through its own SQL
// in insertServiceRolloutTx, which uses starting because the rollout
// engine owns promotion from there.)
func (d *Delivery) wakeSleepingAssignmentsTx(ctx context.Context, tx *sql.Tx, serviceID string, now time.Time) (agentIDs []string, flipped int, err error) {
	allocations, err := d.store.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, true)
	if err != nil {
		return nil, 0, err
	}
	var mutations []allocationMutation
	seen := make(map[string]struct{})
	for _, alloc := range allocations {
		if alloc.RolloutState != AllocationRolloutSleeping {
			continue
		}
		mutations = append(mutations, allocationMutation{Kind: mutationChangeIntent, State: allocationAssignmentState{
			AllocationID: alloc.ID,
			RolloutState: AllocationRolloutServing,
			Intent:       allocationIntentRun,
			UpdatedAt:    now,
		}})
		flipped++
		if _, ok := seen[alloc.AgentID]; !ok {
			seen[alloc.AgentID] = struct{}{}
			agentIDs = append(agentIDs, alloc.AgentID)
		}
	}
	if err := d.applyAllocationMutationsTx(ctx, tx, now, mutations...); err != nil {
		return nil, 0, err
	}
	return agentIDs, flipped, nil
}

// wakeServiceTx wakes a sleeping service: state becomes waking, sleeping
// assignments run again, and replica reconciliation replaces anything lost
// while asleep. An already-awake service only gets its activity stamped, so
// wake demand is itself proof against an imminent sleep. Returns whether a
// sleep→wake transition happened.
func (d *Delivery) wakeServiceTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, now time.Time) (bool, []string, error) {
	s := d.store
	rec, err := s.serverlessRecordTx(ctx, tx, service.ID, true)
	if err != nil {
		return false, nil, err
	}
	if !rec.Found || rec.State == ServerlessAwake {
		if rec.Found {
			rec.LastActivity = sql.NullTime{Time: now, Valid: true}
			if err := s.upsertServerlessRecordTx(ctx, tx, service.ID, rec, now); err != nil {
				return false, nil, err
			}
		}
		return false, nil, nil
	}
	// A billing-stopped service stays down: waking would immediately be
	// undone by reconciliation, and public callers should see a clean 503.
	if restricted, _, err := billing.ServiceEnforcedTx(ctx, tx, service.ID); err != nil {
		return false, nil, err
	} else if restricted {
		return false, nil, nil
	}
	rec.State = ServerlessWaking
	rec.AsleepSince = sql.NullTime{}
	rec.LastActivity = sql.NullTime{Time: now, Valid: true}
	if err := s.upsertServerlessRecordTx(ctx, tx, service.ID, rec, now); err != nil {
		return false, nil, err
	}
	agentIDs, _, err := d.wakeSleepingAssignmentsTx(ctx, tx, service.ID, now)
	if err != nil {
		return false, nil, err
	}
	if _, err := d.reconcileServiceReplicasTx(ctx, tx, service, "", now); err != nil {
		return false, nil, err
	}
	if err := s.recordServerlessTransitionTx(ctx, tx, service, reasonServerlessWake, "service woke on request (serverless)", now); err != nil {
		return false, nil, err
	}
	return true, agentIDs, nil
}

// promoteWakingTx flips waking→awake once any allocation is ready to serve.
// Returns true on transition.
func (d *Delivery) promoteWakingTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, now time.Time) (bool, error) {
	s := d.store
	rec, err := s.serverlessRecordTx(ctx, tx, service.ID, true)
	if err != nil {
		return false, err
	}
	if !rec.Found || rec.State != ServerlessWaking {
		return false, nil
	}
	allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, false)
	if err != nil {
		return false, err
	}
	for _, alloc := range allocations {
		if AllocationReady(alloc) {
			rec.State = ServerlessAwake
			if err := s.upsertServerlessRecordTx(ctx, tx, service.ID, rec, now); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// WakeService wakes the service if it is asleep. All wake triggers (public
// activator, private mesh demand, scale/restart actions, the idle
// evaluator's safety checks) converge here so wake is idempotent.
func (d *Delivery) WakeService(ctx context.Context, serviceID, trigger string) (bool, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	started := time.Now().UTC()
	var woke bool
	var agentIDs []string
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if service.Deletion != nil {
			return nil
		}
		var txErr error
		woke, agentIDs, txErr = d.wakeServiceTx(ctx, tx, service, started)
		return txErr
	})
	if err != nil {
		return false, err
	}
	if woke {
		telemetry.RecordServerlessWake(ctx, trigger)
		for _, agentID := range agentIDs {
			if d.notifier != nil {
				d.notifier.Notify(agentID)
			}
		}
		if d.ingress != nil {
			d.ingress.RequestSync()
		}
	}
	return woke, nil
}

// WakeEndpoint is one ready allocation address for a woken service.
type WakeEndpoint struct {
	Address string
	Port    int32
}

// WakeResult is the outcome of a hostname wake for the ingress activator.
type WakeResult struct {
	ServiceID string
	Endpoints []WakeEndpoint
	Ready     bool
}

var ErrWakeHostnameNotFound = errors.New("no live domain binding for hostname")

// WakeServiceByHostname resolves a public hostname to its service
// server-side, wakes it, and waits until an endpoint for the binding's
// target port is ready or ctx ends. The readiness rule mirrors the ingress
// backend rule exactly, so the activator only ever proxies to an endpoint
// Envoy itself would use.
func (d *Delivery) WakeServiceByHostname(ctx context.Context, hostname string) (WakeResult, error) {
	s := d.store
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	var serviceID string
	var targetPort int32
	err := s.db.QueryRowContext(ctx, `SELECT b.service_id, b.target_port
		FROM domain_bindings b JOIN live_services ls ON ls.id = b.service_id
		WHERE b.hostname = $1 AND b.deleted_at IS NULL`, hostname).Scan(&serviceID, &targetPort)
	if errors.Is(err, sql.ErrNoRows) {
		return WakeResult{}, ErrWakeHostnameNotFound
	}
	if err != nil {
		return WakeResult{}, err
	}
	wakeStarted := time.Now().UTC()
	if _, err := d.WakeService(ctx, serviceID, WakeTriggerPublic); err != nil {
		return WakeResult{}, err
	}
	result := WakeResult{ServiceID: serviceID}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		allocations, err := s.listAllocationsByServiceID(ctx, serviceID)
		if err != nil {
			return WakeResult{}, err
		}
		result.Endpoints = readyWakeEndpoints(allocations, targetPort)
		if len(result.Endpoints) > 0 {
			result.Ready = true
			telemetry.RecordServerlessWakeDuration(ctx, time.Since(wakeStarted))
			// Promotion is also covered by the evaluator; do it inline so
			// status reads right after a wake already report awake.
			_ = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
				service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
				if err != nil {
					return err
				}
				_, err = d.promoteWakingTx(ctx, tx, service, time.Now().UTC())
				return err
			})
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, nil
		case <-ticker.C:
		}
	}
}

func readyWakeEndpoints(allocations []AllocationRecord, targetPort int32) []WakeEndpoint {
	var endpoints []WakeEndpoint
	for _, rec := range allocations {
		if !rec.Healthy || rec.RolloutState != AllocationRolloutServing ||
			rec.AppliedSpecRevision < rec.DesiredSpecRevision ||
			rec.AppliedRolloutGeneration < rec.DesiredRolloutGeneration ||
			rec.Phase == restartpolicy.PhaseCrashLoop {
			continue
		}
		switch {
		case slices.Contains(rec.HealthyIPv4Ports, targetPort) && rec.AllocationIPv4 != "":
			endpoints = append(endpoints, WakeEndpoint{Address: rec.AllocationIPv4, Port: targetPort})
		case slices.Contains(rec.HealthyIPv6Ports, targetPort) && rec.AllocationIPv6 != "":
			endpoints = append(endpoints, WakeEndpoint{Address: rec.AllocationIPv6, Port: targetPort})
		}
	}
	return endpoints
}

// ReportWakeDemands wakes services whose sleeping allocations received
// private mesh traffic on the reporting agent. Demands for allocations not
// assigned to that agent are ignored. Errors are aggregated but individual
// wakes are independent.
func (d *Delivery) ReportWakeDemands(ctx context.Context, agentID string, demands []*agentv1.WakeDemand) error {
	if len(demands) == 0 {
		return nil
	}
	s := d.store
	serviceIDs := make(map[string]struct{})
	for _, demand := range demands {
		var serviceID string
		err := s.db.QueryRowContext(ctx,
			`SELECT service_id FROM allocation_assignments WHERE id = $1 AND agent_id = $2`,
			demand.GetAllocationId(), agentID).Scan(&serviceID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		serviceIDs[serviceID] = struct{}{}
	}
	var firstErr error
	for serviceID := range serviceIDs {
		if _, err := d.WakeService(ctx, serviceID, WakeTriggerPrivate); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// lastNetworkActivityTx returns the end of the latest accepted allocation
// usage interval for the service that carried user network traffic
// (public direct, public ingress, or private egress) at or after since.
// Platform and probe classes never count, so health checks cannot pin a
// service awake.
func (s *persistence) lastNetworkActivityTx(ctx context.Context, q ServiceQueryer, serviceID string, since time.Time) (sql.NullTime, error) {
	var latest sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT MAX(end_at) FROM usage_intervals
		WHERE service_id = $1 AND status = 'accepted' AND resource_kind = 'allocation'
		  AND start_at >= $2
		  AND (egress_public_direct > 0 OR egress_public_ingress > 0 OR egress_private > 0)`,
		serviceID, since).Scan(&latest)
	return latest, err
}

func rolloutInProgressTx(ctx context.Context, q ServiceQueryer, serviceID string, generation int64) (bool, error) {
	if generation <= 0 {
		return false, nil
	}
	var state string
	err := q.QueryRowContext(ctx,
		`SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`,
		serviceID, generation).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == rolloutStateInProgress || state == rolloutStatePendingBuild, nil
}

// EvaluateServerlessIdle is one pass of the idle evaluator: refresh activity
// for awake serverless services, sleep those idle past the timeout, promote
// finished wakes, and enforce the safety rules (never stay asleep across a
// rollout, a billing stop, or a spec that disabled serverless). It runs on
// the singleton maintenance loop.
func (d *Delivery) EvaluateServerlessIdle(ctx context.Context, idleTimeout time.Duration) error {
	if d == nil || d.store == nil {
		return nil
	}
	if idleTimeout <= 0 {
		return fmt.Errorf("serverless idle timeout must be greater than zero")
	}
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	now, err := dbtx.DatabaseTime(ctx, s.db)
	if err != nil {
		return fmt.Errorf("read database time: %w", err)
	}
	now = now.UTC()

	enabled, err := d.serverlessEnabledServices(ctx)
	if err != nil {
		return err
	}
	tracked, err := d.serverlessTrackedServices(ctx)
	if err != nil {
		return err
	}
	candidates := make(map[string]bool, len(enabled)+len(tracked))
	for id := range enabled {
		candidates[id] = true
	}
	for _, id := range tracked {
		if _, ok := candidates[id]; !ok {
			candidates[id] = false
		}
	}
	for serviceID, isEnabled := range candidates {
		slept, woke, agentIDs, err := d.evaluateServerlessService(ctx, serviceID, isEnabled, idleTimeout, now)
		if err != nil {
			return err
		}
		if slept {
			telemetry.RecordServerlessSleep(ctx)
		}
		if woke {
			telemetry.RecordServerlessWake(ctx, WakeTriggerSystem)
		}
		if slept || woke {
			for _, agentID := range agentIDs {
				if d.notifier != nil {
					d.notifier.Notify(agentID)
				}
			}
			if d.ingress != nil {
				d.ingress.RequestSync()
			}
		}
	}
	return nil
}

// serverlessEnabledServices maps services whose *applied* revision (the
// current rollout generation's spec) enables serverless.
func (d *Delivery) serverlessEnabledServices(ctx context.Context) (map[string]bool, error) {
	rows, err := d.store.db.QueryContext(ctx, `SELECT s.id, r.spec_json
		FROM live_services s
		JOIN service_delivery_status ds ON ds.service_id = s.id
		JOIN service_rollouts ro ON ro.service_id = s.id AND ro.rollout_generation = ds.current_rollout_generation
		JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = ro.spec_revision
		WHERE ds.current_rollout_generation IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	enabled := make(map[string]bool)
	for rows.Next() {
		var serviceID string
		var raw []byte
		if err := rows.Scan(&serviceID, &raw); err != nil {
			return nil, err
		}
		spec, err := LoadServiceSpec(raw)
		if err != nil {
			return nil, err
		}
		if spec.GetServerlessEnabled() {
			enabled[serviceID] = true
		}
	}
	return enabled, rows.Err()
}

func (d *Delivery) serverlessTrackedServices(ctx context.Context) ([]string, error) {
	rows, err := d.store.db.QueryContext(ctx, `SELECT service_id FROM service_serverless_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *Delivery) evaluateServerlessService(ctx context.Context, serviceID string, enabled bool, idleTimeout time.Duration, now time.Time) (slept, woke bool, agentIDs []string, err error) {
	s := d.store
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if service.Deletion != nil {
			return nil
		}
		rec, err := s.serverlessRecordTx(ctx, tx, serviceID, true)
		if err != nil {
			return err
		}
		if !enabled {
			// Serverless was disabled in the applied spec: wake and stop
			// tracking. Rollout creation normally wakes first; this is the
			// guarantee catch-all.
			if !rec.Found {
				return nil
			}
			if rec.State != ServerlessAwake {
				var txErr error
				woke, agentIDs, txErr = d.wakeServiceTx(ctx, tx, service, now)
				if txErr != nil {
					return txErr
				}
			}
			return s.deleteServerlessRecordTx(ctx, tx, serviceID)
		}
		inRollout, err := rolloutInProgressTx(ctx, tx, serviceID, service.RolloutGeneration)
		if err != nil {
			return err
		}
		switch rec.State {
		case ServerlessWaking:
			_, err := d.promoteWakingTx(ctx, tx, service, now)
			return err
		case ServerlessAsleep:
			if inRollout {
				var txErr error
				woke, agentIDs, txErr = d.wakeServiceTx(ctx, tx, service, now)
				return txErr
			}
			return nil
		}
		// Awake: refresh activity, then judge idleness.
		since := now.Add(-idleTimeout - time.Hour)
		if rec.LastActivity.Valid && rec.LastActivity.Time.After(since) {
			since = rec.LastActivity.Time
		}
		latest, err := s.lastNetworkActivityTx(ctx, tx, serviceID, since)
		if err != nil {
			return err
		}
		lastActivity := rec.LastActivity
		if latest.Valid && (!lastActivity.Valid || latest.Time.After(lastActivity.Time)) {
			lastActivity = latest
		}
		if !lastActivity.Valid {
			// First observation with no recorded traffic: grant a full
			// idle window from now rather than sleeping a service the
			// evaluator has never actually seen idle.
			lastActivity = sql.NullTime{Time: now, Valid: true}
		}
		if !rec.Found || !rec.LastActivity.Valid || lastActivity.Time.After(rec.LastActivity.Time) {
			rec.Found = true
			rec.State = ServerlessAwake
			rec.LastActivity = lastActivity
			if err := s.upsertServerlessRecordTx(ctx, tx, serviceID, rec, now); err != nil {
				return err
			}
		}
		if inRollout || now.Sub(lastActivity.Time) < idleTimeout {
			return nil
		}
		if restricted, _, err := billing.ServiceEnforcedTx(ctx, tx, serviceID); err != nil {
			return err
		} else if restricted {
			return nil
		}
		// Never sleep a service with nothing ready: a crash-looping or
		// still-starting service emits no traffic, but sleeping it would
		// mask the failure and fight the restart policy.
		allocations, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, false)
		if err != nil {
			return err
		}
		ready := false
		for _, alloc := range allocations {
			if AllocationReady(alloc) {
				ready = true
				break
			}
		}
		if !ready {
			return nil
		}
		agentIDs, err = d.sleepServiceTx(ctx, tx, service, now)
		if err != nil {
			return err
		}
		slept = true
		return nil
	})
	return slept, woke, agentIDs, err
}
