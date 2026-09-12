package delivery

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/journal"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
)

type rolloutRecord struct {
	ServiceID           string
	Generation          int64
	SpecRevision        int64
	State               string
	Strategy            *platformv1.RollingStrategy
	DesiredReplicaCount int32
	ImageDigest         string
	FailureReason       string
	TargetAllocationID  string
	CreatedAt           time.Time
	ProgressAt          time.Time
}

type rolloutAdvanceResult struct {
	Changed                 bool
	IngressChanged          bool
	NeedsIngressConvergence bool
	AgentIDs                []string
	EnvironmentID           string
}

func (d *Delivery) advanceRollout(ctx context.Context, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	s := d.store
	var result rolloutAdvanceResult
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		result, err = d.advanceRolloutTx(ctx, tx, serviceID, now.UTC())
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (d *Delivery) advanceRolloutTx(ctx context.Context, tx *sql.Tx, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
	s := d.store
	result := rolloutAdvanceResult{}
	if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
		return result, err
	}
	service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
	if err != nil {
		return result, err
	}
	result.EnvironmentID = service.EnvironmentID
	rollout, ok, err := loadCurrentRolloutTx(ctx, tx, service)
	if err != nil || !ok {
		return result, err
	}
	allocs, err := s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, true)
	if err != nil {
		return result, err
	}

	removalDeploymentID, removing, err := currentRemovalDeploymentTx(ctx, tx, serviceID)
	if err != nil {
		return result, err
	}
	plan := decideRollout(rolloutSnapshot{Rollout: rollout, Allocations: allocs, Removing: removing}, now)
	result = plan.Result
	result.EnvironmentID = service.EnvironmentID
	for _, alloc := range plan.Remove {
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionCompleteDrain, AllocationID: alloc.ID})); err != nil {
			return result, err
		}
	}
	for _, alloc := range plan.Promote {
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionChangeIntent, State: allocationAssignmentState{
			AllocationID: alloc.ID, RolloutState: AllocationRolloutServing, Intent: allocationIntentRun, UpdatedAt: now,
		}})); err != nil {
			return result, err
		}
	}
	if err := d.persistRolloutWithdrawalsTx(ctx, tx, plan.Withdraw, now); err != nil {
		return result, err
	}
	if plan.CompleteRemoval {
		return result, s.finalizeDeploymentRemovalTx(ctx, tx, serviceID, removalDeploymentID, deploymentActor{Kind: DeploymentCauseSystem}, now)
	}
	if plan.Failure != "" {
		return result, d.failRolloutTx(ctx, tx, service, rollout, plan.FailureTargets, plan.Failure, now)
	}
	if plan.Complete {
		return result, d.completeRolloutTx(ctx, tx, service, rollout, now)
	}
	if !plan.Continue {
		return result, nil
	}

	occupied := make(map[string]struct{}, len(plan.Allocations))
	for _, alloc := range plan.Allocations {
		occupied[alloc.AgentID] = struct{}{}
	}
	created := 0
	for i := 0; i < plan.PlacementSlots; i++ {
		agentID, err := d.chooseReplicaAgentTx(ctx, tx, service, occupied, "", false)
		if errors.Is(err, ErrNoPlacementAvailable) {
			break
		}
		if err != nil {
			return result, err
		}
		alloc, err := d.insertAllocationTx(ctx, tx, service, agentID, now)
		if err != nil {
			return result, err
		}
		occupied[agentID] = struct{}{}
		result.AgentIDs = append(result.AgentIDs, alloc.AgentID)
		result.Changed = true
		created++
	}
	finish := decideRolloutPlacement(rolloutSnapshot{Rollout: rollout, Allocations: plan.Allocations}, created, result.NeedsIngressConvergence, now)
	if err := d.persistRolloutWithdrawalsTx(ctx, tx, finish.Withdraw, now); err != nil {
		return result, err
	}
	result.Changed = result.Changed || finish.Result.Changed
	result.IngressChanged = result.IngressChanged || finish.Result.IngressChanged
	result.NeedsIngressConvergence = finish.Result.NeedsIngressConvergence
	result.AgentIDs = append(result.AgentIDs, finish.Result.AgentIDs...)
	if err := s.setServicePlacementMessageTx(ctx, tx, serviceID, finish.PlacementMessage, now); err != nil {
		return result, err
	}
	if finish.Failure != "" {
		if err := d.failRolloutTx(ctx, tx, service, rollout, finish.FailureTargets, finish.Failure, now); err != nil {
			return result, err
		}
	}
	if result.Changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE service_rollouts SET progress_at = $1 WHERE service_id = $2 AND rollout_generation = $3`,
			now, serviceID, rollout.Generation,
		); err != nil {
			return result, err
		}
		journal.RecordRollout(ctx, serviceID, rollout.Generation)
	}
	if err := d.updateRolloutProgressDetailTx(ctx, tx, serviceID, rollout, now); err != nil {
		return result, err
	}
	return result, nil
}

func (d *Delivery) persistRolloutWithdrawalsTx(ctx context.Context, tx *sql.Tx, withdrawals []rolloutWithdrawal, now time.Time) error {
	for _, withdrawal := range withdrawals {
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionChangeIntent, State: allocationAssignmentState{
			AllocationID: withdrawal.AllocationID, RolloutState: AllocationRolloutWithdrawing,
			Intent: allocationIntentRun, IntentMessage: withdrawal.Message, UpdatedAt: now,
		}})); err != nil {
			return err
		}
	}
	return nil
}

func (d *Delivery) confirmRolloutIngressConverged(ctx context.Context, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
	s := d.store
	result := rolloutAdvanceResult{}
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		result = rolloutAdvanceResult{}
		if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
			return err
		}
		service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		result.EnvironmentID = service.EnvironmentID
		rollout, ok, err := loadCurrentRolloutTx(ctx, tx, service)
		if err != nil || !ok || rollout.State != rolloutStateInProgress {
			return err
		}
		_, removing, err := currentRemovalDeploymentTx(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		if (!ok || rollout.State != rolloutStateInProgress) && !removing {
			return nil
		}
		strategy := canonicalRollingStrategy(nil)
		if ok {
			strategy = rollout.Strategy
		}
		deadline := now.UTC().Add(time.Duration(strategy.GetDrainingSeconds()) * time.Second)
		rows, err := tx.QueryContext(ctx,
			`SELECT id, agent_id FROM allocation_assignments
			  WHERE service_id = $1 AND rollout_state = $2
			  ORDER BY created_at, id FOR UPDATE`,
			serviceID, AllocationRolloutWithdrawing,
		)
		if err != nil {
			return err
		}
		type withdrawingAllocation struct{ id, agentID string }
		var withdrawing []withdrawingAllocation
		for rows.Next() {
			var alloc withdrawingAllocation
			if err := rows.Scan(&alloc.id, &alloc.agentID); err != nil {
				_ = rows.Close()
				return err
			}
			withdrawing = append(withdrawing, alloc)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(withdrawing) == 0 {
			return nil
		}
		for _, alloc := range withdrawing {
			if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionBeginDrain, State: allocationAssignmentState{
				AllocationID: alloc.id, RolloutState: AllocationRolloutDraining, Intent: allocationIntentDrain,
				IntentMessage: "ingress converged; gracefully draining",
				DrainStarted:  sql.NullTime{Time: now.UTC(), Valid: true},
				DrainDeadline: sql.NullTime{Time: deadline, Valid: true}, UpdatedAt: now.UTC(),
			}})); err != nil {
				return err
			}
			result.AgentIDs = append(result.AgentIDs, alloc.agentID)
		}
		if !removing {
			if err := d.markPredecessorDeploymentsDrainingTx(ctx, tx, serviceID, rollout.Generation, now.UTC()); err != nil {
				return err
			}
		}
		if ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE service_rollouts SET progress_at = $1 WHERE service_id = $2 AND rollout_generation = $3`,
				now.UTC(), serviceID, rollout.Generation,
			); err != nil {
				return err
			}
			journal.RecordRollout(ctx, serviceID, rollout.Generation)
		}
		result.Changed = true
		return nil
	})
	return result, err
}

func currentRemovalDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) (string, bool, error) {
	var deploymentID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE AND state = $2 AND reason_code = $3
		  LIMIT 1 FOR UPDATE`,
		serviceID, DeploymentStateDraining, reasonUserRemove,
	).Scan(&deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return deploymentID, err == nil, err
}

func loadCurrentRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord) (rolloutRecord, bool, error) {
	var rec rolloutRecord
	var strategyRaw []byte
	err := tx.QueryRowContext(ctx,
		`SELECT service_id, rollout_generation, spec_revision, state, strategy_json,
		        desired_replica_count, image_digest, failure_reason, COALESCE(target_allocation_id, ''), created_at, progress_at
		   FROM service_rollouts
		  WHERE service_id = $1 AND rollout_generation = $2
		  FOR UPDATE`, service.ID, service.RolloutGeneration,
	).Scan(&rec.ServiceID, &rec.Generation, &rec.SpecRevision, &rec.State, &strategyRaw,
		&rec.DesiredReplicaCount, &rec.ImageDigest, &rec.FailureReason, &rec.CreatedAt, &rec.ProgressAt)
	if err == sql.ErrNoRows {
		return rolloutRecord{}, false, nil
	}
	if err != nil {
		return rolloutRecord{}, false, err
	}
	rec.Strategy = &platformv1.RollingStrategy{}
	if len(strategyRaw) > 0 && string(strategyRaw) != "{}" && string(strategyRaw) != "null" {
		if err := protojson.Unmarshal(strategyRaw, rec.Strategy); err != nil {
			return rolloutRecord{}, false, err
		}
	}
	rec.Strategy = canonicalRollingStrategy(rec.Strategy)
	return rec, true, nil
}

func (d *Delivery) failRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, rollout rolloutRecord, target []AllocationRecord, reason string, now time.Time) error {
	s := d.store
	deadline := now.Add(time.Duration(rollout.Strategy.GetDrainingSeconds()) * time.Second)
	for _, alloc := range target {
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionBeginDrain, State: allocationAssignmentState{
			AllocationID: alloc.ID, RolloutState: AllocationRolloutDraining, Intent: allocationIntentDrain,
			IntentMessage: "failed replacement; cleaning up",
			DrainStarted:  sql.NullTime{Time: now, Valid: true},
			DrainDeadline: sql.NullTime{Time: deadline, Valid: true}, UpdatedAt: now,
		}})); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts SET state = $1, failure_reason = $2, completed_at = $3, progress_at = $3
		  WHERE service_id = $4 AND rollout_generation = $5`,
		rolloutStateFailed, sanitizeDeploymentDetail(reason), now, service.ID, rollout.Generation,
	); err != nil {
		return err
	}
	journal.RecordRollout(ctx, service.ID, rollout.Generation)
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState:          DeploymentStateFailed,
		Actor:            deploymentActor{Kind: DeploymentCauseSystem},
		ReasonCode:       reasonDeploymentFailed,
		Detail:           reason,
		IgnoreIfTerminal: true,
	})
	return err
}

func (d *Delivery) completeRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, rollout rolloutRecord, now time.Time) error {
	s := d.store
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts SET state = $1, failure_reason = '', completed_at = $2, progress_at = $2
		  WHERE service_id = $3 AND rollout_generation = $4`,
		rolloutStateSucceeded, now, service.ID, rollout.Generation,
	); err != nil {
		return err
	}
	journal.RecordRollout(ctx, service.ID, rollout.Generation)
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	if _, err := s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState:          DeploymentStateActive,
		Actor:            deploymentActor{Kind: DeploymentCauseSystem},
		ReasonCode:       reasonDeploymentActive,
		Detail:           fmt.Sprintf("Rollout complete: %d of %d replacement replicas ready", rolloutTargetReplicaCount(rollout), rolloutTargetReplicaCount(rollout)),
		IgnoreIfTerminal: true,
	}); err != nil {
		return err
	}
	if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
		return err
	}
	return s.completeDrainedPredecessorsTx(ctx, tx, service.ID, dep.ID, deploymentActor{Kind: DeploymentCauseSystem})
}

func (d *Delivery) markPredecessorDeploymentsDrainingTx(ctx context.Context, tx *sql.Tx, serviceID string, generation int64, now time.Time) error {
	s := d.store
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM deployments WHERE service_id = $1 AND rollout_generation < $2 AND state = $3 FOR UPDATE`,
		serviceID, generation, DeploymentStateActive)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.applyDeploymentTransitionTx(ctx, tx, id, deploymentTransitionInput{
			ToState: DeploymentStateDraining, Actor: deploymentActor{Kind: DeploymentCauseSystem},
			ReasonCode: reasonDeploymentDraining, Detail: "Healthy replacements entered ingress; predecessor is draining",
		}); err != nil {
			return err
		}
	}
	_ = now
	return nil
}

func (d *Delivery) updateRolloutProgressDetailTx(ctx context.Context, tx *sql.Tx, serviceID string, rollout rolloutRecord, now time.Time) error {
	var ready, starting, draining int
	if err := tx.QueryRowContext(ctx,
		`SELECT
		   count(*) FILTER (WHERE desired_rollout_generation = $2 AND rollout_state = 'serving' AND healthy),
		   count(*) FILTER (WHERE desired_rollout_generation = $2 AND rollout_state = 'starting'),
		   count(*) FILTER (WHERE rollout_state = 'draining')
		 FROM allocations WHERE service_id = $1`, serviceID, rollout.Generation,
	).Scan(&ready, &starting, &draining); err != nil {
		return err
	}
	detail := fmt.Sprintf("Rolling replacement: %d/%d ready, %d starting, %d draining", ready, rolloutTargetReplicaCount(rollout), starting, draining)
	rows, err := tx.QueryContext(ctx,
		`UPDATE deployments SET detail = $1, updated_at = $2
		  WHERE service_id = $3 AND rollout_generation = $4 AND is_current = TRUE AND state NOT IN ('failed','active')
		    AND detail IS DISTINCT FROM $1
		  RETURNING id`,
		detail, now, serviceID, rollout.Generation)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		journal.RecordDeployment(ctx, id)
	}
	return rows.Err()
}

func (d *Delivery) prepareReplacementRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, existing []AllocationRecord, now time.Time) (bool, error) {
	s := d.store
	if err := s.lockServiceTx(ctx, tx, service.ID); err != nil {
		return false, err
	}
	if ServiceVolumeName(service.Spec) != "" && len(existing) > 0 {
		return false, ErrVolumeRollingUnsupported
	}
	rollout, ok, err := loadCurrentRolloutTx(ctx, tx, service)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	switch rollout.State {
	case rolloutStateInProgress, rolloutStatePendingBuild:
	default:
		return false, nil
	}
	return true, d.supersedeCurrentRolloutTx(ctx, tx, service, rollout, existing, now)
}

func (d *Delivery) supersedeCurrentRolloutTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, rollout rolloutRecord, existing []AllocationRecord, now time.Time) error {
	s := d.store
	for _, alloc := range existing {
		if alloc.RolloutState != AllocationRolloutStarting {
			continue
		}
		if err := d.applySchedulingPlanTx(ctx, tx, allocationMutationPlan(now, SchedulingDecision{Kind: DecisionCompleteDrain, AllocationID: alloc.ID})); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts SET state = $1, failure_reason = $2, completed_at = $3, progress_at = $3
		  WHERE service_id = $4 AND rollout_generation = $5`,
		rolloutStateSuperseded, "superseded by a newer rollout", now, service.ID, rollout.Generation,
	); err != nil {
		return err
	}
	journal.RecordRollout(ctx, service.ID, rollout.Generation)
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState:          DeploymentStateSuperseded,
		Actor:            deploymentActor{Kind: DeploymentCauseSystem},
		ReasonCode:       reasonDeploymentSuperseded,
		Detail:           "Superseded by a newer rollout",
		IgnoreIfTerminal: true,
	})
	return err
}
