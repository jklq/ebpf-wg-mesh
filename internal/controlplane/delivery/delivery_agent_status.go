package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/restartpolicy"
)

func (d *Delivery) ObserveAgentStatus(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) error {
	ingressChanged, _, err := d.recordStatusReport(ctx, authenticatedAgentID, report)
	if err != nil {
		return err
	}
	if ingressChanged && d.ingress != nil && d.live.Publishing() {
		d.ingress.RequestSync()
	}
	return nil
}

func (d *Delivery) recordStatusReport(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) (bool, []string, error) {
	if !d.live.Serving() {
		return false, nil, ErrNotLiveOwner
	}
	if report == nil || strings.TrimSpace(report.GetAgentId()) == "" || report.GetAgentId() != authenticatedAgentID {
		return false, nil, fmt.Errorf("%w: report agent_id does not match authenticated agent", ErrAllocationOwnership)
	}
	inventory := make([]string, 0, len(report.GetServices()))
	for _, cond := range report.GetServices() {
		inventory = append(inventory, cond.GetAllocationId())
	}

	durable := d.live.Durable()
	if err := validateStatusInventory(durable, authenticatedAgentID, report); err != nil {
		return false, nil, err
	}
	if err := d.live.AcceptReport(authenticatedAgentID, report.GetSessionId(), report.GetObservationSequence(), inventory, !report.GetRecoveryMode()); err != nil {
		return false, nil, err
	}

	now := d.live.currentTime()
	ingressChanged := false
	statusInvalidated := false
	stateChanged := false
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	deploymentAllocationIDs := make(map[string]struct{})
	for _, cond := range report.GetServices() {
		assignment := durable.Assignments[cond.GetAllocationId()]
		generation := cond.GetDesiredRolloutGeneration()

		phase, healthy := cond.GetPhase(), cond.GetHealthy()
		if cond.GetRestart().GetCrashLoop() || phase == restartpolicy.PhaseCrashLoop {
			phase, healthy = restartpolicy.PhaseCrashLoop, false
		}
		observation := AllocationObservation{
			AllocationID: cond.GetAllocationId(), RolloutGeneration: generation,
			AppliedSpecRevision: cond.GetAppliedSpecRevision(), AppliedGeneration: cond.GetAppliedRolloutGeneration(),
			Phase: phase, Message: cond.GetMessage(), Healthy: healthy,
			HealthyIPv4Ports: cond.GetHealthyIpv4Ports(), HealthyIPv6Ports: cond.GetHealthyIpv6Ports(), Restart: cond.GetRestart(),
			AgentID: authenticatedAgentID, SessionID: report.GetSessionId(), Sequence: report.GetObservationSequence(), ObservedAt: now,
		}
		outcome, err := d.live.RecordObservation(observation)
		if err != nil {
			return false, nil, err
		}
		// Only the desired generation renders into service status; stale
		// generations never invalidate subscribers.
		if outcome.StatusInvalidated && generation == assignment.DesiredRolloutGeneration {
			statusInvalidated = true
		}
		if !outcome.Changed || generation != assignment.DesiredRolloutGeneration {
			continue
		}
		service := durable.Services[assignment.ServiceID]
		changedEnvironments[service.EnvironmentID] = struct{}{}
		rollout := durable.Rollouts[fmt.Sprintf("%s/%d", assignment.ServiceID, generation)]
		if rollout.State == rolloutStateInProgress {
			rolloutServiceIDs[assignment.ServiceID] = struct{}{}
		} else {
			deploymentAllocationIDs[cond.GetAllocationId()] = struct{}{}
		}
		if _, hasDomain := domainForService(durable, assignment.ServiceID); hasDomain {
			ingressChanged = true
		}
	}
	for allocationID := range deploymentAllocationIDs {
		changed, err := d.evaluateObservedDeployment(ctx, allocationID)
		if err != nil {
			return false, nil, fmt.Errorf("evaluate deployment after observation: %w", err)
		}
		stateChanged = stateChanged || changed
	}
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := d.advanceRollout(ctx, serviceID, now)
		if advanceErr != nil {
			return false, nil, fmt.Errorf("advance rollout after status: %w", advanceErr)
		}
		stateChanged = stateChanged || advanced.Changed
		ingressChanged = ingressChanged || advanced.IngressChanged
		if advanced.EnvironmentID != "" {
			changedEnvironments[advanced.EnvironmentID] = struct{}{}
		}
	}
	if statusInvalidated && !stateChanged {
		// The rendered status changed but no transaction touched durable
		// state (evidence-only updates, or a deployment/rollout evaluation
		// that produced no transition, such as a terminal deployment), so
		// no revision was bumped; publish an explicit invalidation, otherwise
		// status subscribers keep showing the older rendered status.
		if err := d.publishStatusInvalidation(ctx); err != nil {
			return false, nil, fmt.Errorf("publish status invalidation: %w", err)
		}
	}
	environmentIDs := make([]string, 0, len(changedEnvironments))
	for environmentID := range changedEnvironments {
		environmentIDs = append(environmentIDs, environmentID)
	}
	return ingressChanged, environmentIDs, nil
}

// publishStatusInvalidation wakes service-status subscribers after observation
// changes that produced no durable state change. The live observation already
// holds the fresh evidence; the revision bump is the invalidation signal.
func (d *Delivery) publishStatusInvalidation(ctx context.Context) error {
	if d.store == nil || d.store.withObservationTx == nil {
		return nil
	}
	return d.store.withObservationTx(ctx, func(context.Context, *sql.Tx) (bool, error) {
		return true, nil
	})
}

func validateStatusInventory(durable journal.DurableState, authenticatedAgentID string, report *agentv1.StatusReport) error {
	for _, cond := range report.GetServices() {
		if cond.GetAllocationId() == "" || cond.GetServiceId() == "" {
			return fmt.Errorf("%w: allocation_id and service_id are required", ErrAllocationOwnership)
		}
		assignment, ok := durable.Assignments[cond.GetAllocationId()]
		if !ok || assignment.AgentID != authenticatedAgentID {
			return fmt.Errorf("%w: allocation %q", ErrAllocationOwnership, cond.GetAllocationId())
		}
		if cond.GetServiceId() != assignment.ServiceID {
			return fmt.Errorf("%w: allocation %q belongs to service %q", ErrAllocationOwnership, cond.GetAllocationId(), assignment.ServiceID)
		}
		generation := cond.GetDesiredRolloutGeneration()
		if generation <= 0 || generation > assignment.DesiredRolloutGeneration {
			return fmt.Errorf("%w: allocation %q generation %d is not assigned (current %d)", ErrAllocationOwnership, cond.GetAllocationId(), generation, assignment.DesiredRolloutGeneration)
		}
		if cond.GetAppliedRolloutGeneration() > generation {
			return fmt.Errorf("%w: applied generation %d exceeds observed generation %d", ErrAllocationOwnership, cond.GetAppliedRolloutGeneration(), generation)
		}
		if generation == assignment.DesiredRolloutGeneration && (cond.GetDesiredSpecRevision() != assignment.DesiredSpecRevision || cond.GetAppliedSpecRevision() > assignment.DesiredSpecRevision) {
			return fmt.Errorf("%w: allocation %q spec revision %d/%d does not match assignment %d", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetDesiredSpecRevision(), cond.GetAppliedSpecRevision(), assignment.DesiredSpecRevision)
		}
		if strings.TrimSpace(cond.GetAllocationIpv4()) != assignment.AllocationIPv4 || strings.TrimSpace(cond.GetAllocationIpv6()) != assignment.AllocationIPv6 {
			return fmt.Errorf("%w: allocation %s reported addresses %q/%q, assigned %q/%q", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetAllocationIpv4(), cond.GetAllocationIpv6(), assignment.AllocationIPv4, assignment.AllocationIPv6)
		}
	}
	return nil
}

func domainForService(durable journal.DurableState, serviceID string) (journal.Domain, bool) {
	for _, domain := range durable.Domains {
		if domain.ServiceID == serviceID {
			return domain, true
		}
	}
	return journal.Domain{}, false
}

// evaluateObservedDeployment folds a live observation into the deployment
// record and reports whether durable state changed. A terminal deployment
// (or a stale observation) yields no transition, in which case the
// transaction bumps no revision and the caller must invalidate status
// subscribers explicitly.
func (d *Delivery) evaluateObservedDeployment(ctx context.Context, allocationID string) (bool, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	var changed bool
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var serviceID, agentID string
		var desired int64
		err := tx.QueryRowContext(ctx, `SELECT service_id, agent_id, desired_rollout_generation
			FROM allocation_assignments WHERE id = $1`, allocationID).Scan(&serviceID, &agentID, &desired)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		obs, ok := d.live.Observation(allocationID, desired)
		if !ok {
			return nil
		}
		session, ok := d.live.Session(agentID)
		if !ok || session.SessionID != obs.SessionID || obs.AgentID != agentID {
			return nil
		}
		transitioned, err := d.store.applyAgentDeploymentObservationTx(ctx, tx, serviceID, desired, obs.Phase, obs.Message, obs.Healthy, obs.AppliedGeneration, agentID)
		if err != nil {
			return err
		}
		changed = transitioned
		return nil
	})
	return changed, err
}
