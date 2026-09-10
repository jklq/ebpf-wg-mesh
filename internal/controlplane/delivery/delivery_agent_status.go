package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/restartpolicy"
)

func (d *Delivery) ObserveAgentStatus(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) error {
	ingressChanged, _, err := d.recordStatusReport(ctx, authenticatedAgentID, report)
	if err != nil {
		return err
	}
	if ingressChanged && d.ingress != nil {
		d.ingress.RequestSync()
	}
	return nil
}

func (d *Delivery) recordStatusReport(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) (bool, []string, error) {
	if report == nil || strings.TrimSpace(report.GetAgentId()) == "" || report.GetAgentId() != authenticatedAgentID {
		return false, nil, fmt.Errorf("%w: report agent_id does not match authenticated agent", ErrAllocationOwnership)
	}
	if report.GetObservationSequence() == 0 || report.GetObservationSequence() > math.MaxInt64 {
		return false, nil, fmt.Errorf("%w: observation_sequence must be between 1 and %d", ErrStaleObservation, int64(math.MaxInt64))
	}

	s := d.store
	var ingressChanged bool
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	deploymentAllocationIDs := make(map[string]struct{})
	err := s.withObservationTx(ctx, func(tx *sql.Tx) error {
		ingressChanged = false
		changedEnvironments = make(map[string]struct{})
		rolloutServiceIDs = make(map[string]struct{})
		deploymentAllocationIDs = make(map[string]struct{})
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.acceptAgentReportTx(ctx, tx, authenticatedAgentID, report.GetSessionId(), report.GetObservationSequence(), !report.GetRecoveryMode(), now); err != nil {
			return err
		}

		for _, cond := range report.GetServices() {
			if cond.GetAllocationId() == "" || cond.GetServiceId() == "" {
				return fmt.Errorf("%w: allocation_id and service_id are required", ErrAllocationOwnership)
			}
			target, err := s.allocationObservationTargetTx(ctx, tx, cond.GetAllocationId(), authenticatedAgentID, cond.GetDesiredRolloutGeneration())
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: allocation %q", ErrAllocationOwnership, cond.GetAllocationId())
			}
			if err != nil {
				return fmt.Errorf("load allocation assignment: %w", err)
			}
			if cond.GetServiceId() != target.serviceID {
				return fmt.Errorf("%w: allocation %q belongs to service %q", ErrAllocationOwnership, cond.GetAllocationId(), target.serviceID)
			}
			generation := cond.GetDesiredRolloutGeneration()
			if generation <= 0 || generation > target.desiredGeneration {
				return fmt.Errorf("%w: allocation %q generation %d is not assigned (current %d)", ErrAllocationOwnership, cond.GetAllocationId(), generation, target.desiredGeneration)
			}
			if cond.GetAppliedRolloutGeneration() > generation {
				return fmt.Errorf("%w: applied generation %d exceeds observed generation %d", ErrAllocationOwnership, cond.GetAppliedRolloutGeneration(), generation)
			}
			if generation == target.desiredGeneration && (cond.GetDesiredSpecRevision() != target.desiredSpecRevision || cond.GetAppliedSpecRevision() > target.desiredSpecRevision) {
				return fmt.Errorf("%w: allocation %q spec revision %d/%d does not match assignment %d", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetDesiredSpecRevision(), cond.GetAppliedSpecRevision(), target.desiredSpecRevision)
			}
			if strings.TrimSpace(cond.GetAllocationIpv4()) != target.assignedIPv4 || strings.TrimSpace(cond.GetAllocationIpv6()) != target.assignedIPv6 {
				return fmt.Errorf("%w: allocation %s reported addresses %q/%q, assigned %q/%q", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetAllocationIpv4(), cond.GetAllocationIpv6(), target.assignedIPv4, target.assignedIPv6)
			}

			restartRaw, err := encodeRestartObservation(cond.GetRestart())
			if err != nil {
				return fmt.Errorf("encode restart observation: %w", err)
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
			if err := s.recordAllocationObservationTx(ctx, tx, observation); err != nil {
				return fmt.Errorf("record allocation observation: %w", err)
			}

			// Older-generation observations remain useful diagnostics, but they
			// cannot affect current readiness, ingress, or deployment history.
			if generation != target.desiredGeneration {
				continue
			}
			statusChanged := !target.previousAppliedSpec.Valid || target.previousAppliedSpec.Int64 != cond.GetAppliedSpecRevision() ||
				!target.previousAppliedGeneration.Valid || target.previousAppliedGeneration.Int64 != cond.GetAppliedRolloutGeneration() ||
				!target.previousPhase.Valid || target.previousPhase.String != phase || !target.previousMessage.Valid || target.previousMessage.String != cond.GetMessage() ||
				!target.previousHealthy.Valid || target.previousHealthy.Bool != healthy ||
				!slices.Equal(target.previousIPv4Ports, cond.GetHealthyIpv4Ports()) || !slices.Equal(target.previousIPv6Ports, cond.GetHealthyIpv6Ports()) ||
				string(target.previousRestartRaw) != string(restartRaw)
			if !statusChanged {
				continue
			}
			changedEnvironments[target.environmentID] = struct{}{}
			var rolloutState string
			err = tx.QueryRowContext(ctx, `SELECT state FROM service_rollouts WHERE service_id = $1 AND rollout_generation = $2`, target.serviceID, generation).Scan(&rolloutState)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if rolloutState == rolloutStateInProgress {
				rolloutServiceIDs[target.serviceID] = struct{}{}
			} else {
				deploymentAllocationIDs[cond.GetAllocationId()] = struct{}{}
			}
			if target.hasDomain && (!target.previousHealthy.Valid || target.previousHealthy.Bool != healthy || !slices.Equal(target.previousIPv4Ports, cond.GetHealthyIpv4Ports()) || !slices.Equal(target.previousIPv6Ports, cond.GetHealthyIpv6Ports()) || !target.previousPhase.Valid || target.previousPhase.String != phase) {
				ingressChanged = true
			}
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	for allocationID := range deploymentAllocationIDs {
		if err := d.evaluateObservedDeployment(ctx, allocationID); err != nil {
			return false, nil, fmt.Errorf("evaluate deployment after observation: %w", err)
		}
	}
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := d.advanceRollout(ctx, serviceID, time.Now().UTC())
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

// Observations request evaluation; only this separately fenced transaction
// decides a deployment transition. Re-read the current session and generation
// so a delayed evaluation cannot apply an obsolete health sample.
func (d *Delivery) evaluateObservedDeployment(ctx context.Context, allocationID string) error {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	return d.store.withTx(ctx, func(tx *sql.Tx) error {
		var serviceID, agentID, phase, message string
		var desired, applied int64
		var healthy bool
		err := tx.QueryRowContext(ctx, `SELECT a.service_id, a.agent_id,
			a.desired_rollout_generation, o.applied_rollout_generation, o.phase, o.message, o.healthy
			FROM allocation_assignments a
			JOIN allocation_observations o ON o.allocation_id = a.id AND o.rollout_generation = a.desired_rollout_generation
			JOIN agent_presence p ON p.agent_id = a.agent_id AND p.session_id = o.session_id
			WHERE a.id = $1`, allocationID).Scan(&serviceID, &agentID, &desired, &applied, &phase, &message, &healthy)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return d.store.applyAgentDeploymentObservationTx(ctx, tx, serviceID, desired, phase, message, healthy, applied, agentID)
	})
}
