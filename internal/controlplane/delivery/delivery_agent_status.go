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
	if report == nil || strings.TrimSpace(report.GetAgentId()) == "" || report.GetAgentId() != authenticatedAgentID {
		return false, nil, fmt.Errorf("%w: report agent_id does not match authenticated agent", ErrAllocationOwnership)
	}
	if err := d.live.AcceptReport(authenticatedAgentID, report.GetSessionId(), report.GetObservationSequence(), !report.GetRecoveryMode()); err != nil {
		return false, nil, err
	}

	durable := d.live.Durable()

	now := d.live.currentTime()
	ingressChanged := false
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	deploymentAllocationIDs := make(map[string]struct{})
	for _, cond := range report.GetServices() {
		if cond.GetAllocationId() == "" || cond.GetServiceId() == "" {
			return false, nil, fmt.Errorf("%w: allocation_id and service_id are required", ErrAllocationOwnership)
		}
		assignment, ok := durable.Assignments[cond.GetAllocationId()]
		if !ok || assignment.AgentID != authenticatedAgentID {
			return false, nil, fmt.Errorf("%w: allocation %q", ErrAllocationOwnership, cond.GetAllocationId())
		}
		if cond.GetServiceId() != assignment.ServiceID {
			return false, nil, fmt.Errorf("%w: allocation %q belongs to service %q", ErrAllocationOwnership, cond.GetAllocationId(), assignment.ServiceID)
		}
		generation := cond.GetDesiredRolloutGeneration()
		if generation <= 0 || generation > assignment.DesiredRolloutGeneration {
			return false, nil, fmt.Errorf("%w: allocation %q generation %d is not assigned (current %d)", ErrAllocationOwnership, cond.GetAllocationId(), generation, assignment.DesiredRolloutGeneration)
		}
		if cond.GetAppliedRolloutGeneration() > generation {
			return false, nil, fmt.Errorf("%w: applied generation %d exceeds observed generation %d", ErrAllocationOwnership, cond.GetAppliedRolloutGeneration(), generation)
		}
		if generation == assignment.DesiredRolloutGeneration && (cond.GetDesiredSpecRevision() != assignment.DesiredSpecRevision || cond.GetAppliedSpecRevision() > assignment.DesiredSpecRevision) {
			return false, nil, fmt.Errorf("%w: allocation %q spec revision %d/%d does not match assignment %d", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetDesiredSpecRevision(), cond.GetAppliedSpecRevision(), assignment.DesiredSpecRevision)
		}
		if strings.TrimSpace(cond.GetAllocationIpv4()) != assignment.AllocationIPv4 || strings.TrimSpace(cond.GetAllocationIpv6()) != assignment.AllocationIPv6 {
			return false, nil, fmt.Errorf("%w: allocation %s reported addresses %q/%q, assigned %q/%q", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetAllocationIpv4(), cond.GetAllocationIpv6(), assignment.AllocationIPv4, assignment.AllocationIPv6)
		}

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
		changed, err := d.live.RecordObservation(observation)
		if err != nil {
			return false, nil, err
		}
		if !changed || generation != assignment.DesiredRolloutGeneration {
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
		if err := d.evaluateObservedDeployment(ctx, allocationID); err != nil {
			return false, nil, fmt.Errorf("evaluate deployment after observation: %w", err)
		}
	}
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := d.advanceRollout(ctx, serviceID, now)
		if advanceErr != nil {
			return false, nil, fmt.Errorf("advance rollout after status: %w", advanceErr)
		}
		ingressChanged = ingressChanged || advanced.IngressChanged
		if advanced.EnvironmentID != "" {
			changedEnvironments[advanced.EnvironmentID] = struct{}{}
		}
	}
	environmentIDs := make([]string, 0, len(changedEnvironments))
	for environmentID := range changedEnvironments {
		environmentIDs = append(environmentIDs, environmentID)
	}
	return ingressChanged, environmentIDs, nil
}

func domainForService(durable journal.DurableState, serviceID string) (journal.Domain, bool) {
	for _, domain := range durable.Domains {
		if domain.ServiceID == serviceID {
			return domain, true
		}
	}
	return journal.Domain{}, false
}

// Observations request evaluation; only this separately fenced transaction
// decides a deployment transition. Re-read the current session and generation
// so a delayed evaluation cannot apply an obsolete health sample.
func (d *Delivery) evaluateObservedDeployment(ctx context.Context, allocationID string) error {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	return d.store.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		return d.store.applyAgentDeploymentObservationTx(ctx, tx, serviceID, desired, obs.Phase, obs.Message, obs.Healthy, obs.AppliedGeneration, agentID)
	})
}
