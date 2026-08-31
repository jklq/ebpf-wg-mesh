package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
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

func (s *Store) advanceRollout(ctx context.Context, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
	var result rolloutAdvanceResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.advanceRolloutTx(ctx, tx, serviceID, now.UTC())
		if err != nil {
			return err
		}
		if result.Changed {
			return s.bumpAllDesiredRevisionsTx(ctx, tx)
		}
		return nil
	})
	return result, err
}

func (s *Store) advanceRolloutTx(ctx context.Context, tx *sql.Tx, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
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

	// A drained allocation is no longer desired. An expired drain deadline is
	// also safe to remove: the agent can only observe the removal at or after
	// that absolute deadline and will force-delete a survivor then.
	kept := allocs[:0]
	for _, alloc := range allocs {
		drained := alloc.RolloutState == allocationRolloutDraining &&
			(alloc.Phase == "Drained" || (alloc.DrainDeadline.Valid && !now.Before(alloc.DrainDeadline.Time)))
		if !drained {
			kept = append(kept, alloc)
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE id = $1`, alloc.ID); err != nil {
			return result, err
		}
		result.Changed = true
		result.AgentIDs = append(result.AgentIDs, alloc.AgentID)
	}
	allocs = kept
	removalDeploymentID, removing, err := currentRemovalDeploymentTx(ctx, tx, serviceID)
	if err != nil {
		return result, err
	}

	// Withdrawal is a durable ingress barrier. The old process keeps running
	// until the reconciler has synchronously applied a routing snapshot that no
	// longer contains it; only then is SIGTERM made desired.
	for _, alloc := range allocs {
		if alloc.RolloutState == allocationRolloutWithdrawing {
			result.NeedsIngressConvergence = true
			result.IngressChanged = true
			return result, nil
		}
	}
	if removing {
		if len(allocs) == 0 {
			if err := s.finalizeDeploymentRemovalTx(ctx, tx, serviceID, removalDeploymentID, deploymentActor{Kind: deploymentCauseSystem}, now); err != nil {
				return result, err
			}
			result.Changed = true
			result.IngressChanged = true
		}
		return result, nil
	}
	if rollout.State != rolloutStateInProgress {
		return result, nil
	}

	target, predecessors, unaffected := splitRolloutAllocations(allocs, rollout)
	desiredTargetCount := rollout.DesiredReplicaCount
	if rollout.TargetAllocationID != "" {
		desiredTargetCount = 1
	}

	for _, alloc := range target {
		if alloc.RolloutState != allocationRolloutStarting || allocationReady(alloc) {
			continue
		}
		failure := rolloutAllocationFailure(alloc, now, rollout.Strategy)
		if failure == "" {
			continue
		}
		if err := s.failRolloutTx(ctx, tx, service, rollout, target, failure, now); err != nil {
			return result, err
		}
		result.Changed = true
		result.AgentIDs = appendAllocationAgentIDs(result.AgentIDs, target...)
		return result, nil
	}

	readyStarting := filterAllocations(target, func(a allocationRecord) bool {
		return a.RolloutState == allocationRolloutStarting && allocationReady(a)
	})
	servingTarget := filterAllocations(target, func(a allocationRecord) bool {
		return a.RolloutState == allocationRolloutServing && allocationReady(a)
	})
	servingOld := filterAllocations(predecessors, func(a allocationRecord) bool {
		return a.RolloutState == allocationRolloutServing
	})
	sortAllocationsStable(readyStarting)
	sortAllocationsStable(servingOld)

	promote := len(readyStarting)
	if len(predecessors) > 0 {
		needed := int(desiredTargetCount) - len(servingTarget)
		if needed < 0 {
			needed = 0
		}
		withoutPredecessor := needed - len(servingOld)
		if withoutPredecessor < 0 {
			withoutPredecessor = 0
		}
		capPromote := len(servingOld) + withoutPredecessor
		if promote > capPromote {
			promote = capPromote
		}
	}
	if promote > 0 {
		for index := 0; index < promote; index++ {
			newAlloc := readyStarting[index]
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations SET rollout_state = $1, updated_at = $2 WHERE id = $3`,
				allocationRolloutServing, now, newAlloc.ID,
			); err != nil {
				return result, err
			}
			servingTarget = append(servingTarget, newAlloc)
			result.AgentIDs = append(result.AgentIDs, newAlloc.AgentID)
			if index >= len(servingOld) {
				continue
			}
			oldAlloc := servingOld[index]
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET rollout_state = $1, phase = 'Withdrawing',
				        message = 'replacement is ready; waiting for ingress withdrawal',
				        updated_at = $2
				  WHERE id = $3`,
				allocationRolloutWithdrawing, now, oldAlloc.ID,
			); err != nil {
				return result, err
			}
			result.IngressChanged = true
			result.NeedsIngressConvergence = true
		}
		result.Changed = true
	}

	// Re-read after promotion so completion and surge accounting use the
	// transaction's durable view rather than local guesses.
	allocs, err = s.listAllocationsByServiceIDQuerier(ctx, tx, serviceID, true)
	if err != nil {
		return result, err
	}
	target, predecessors, unaffected = splitRolloutAllocations(allocs, rollout)
	servingTarget = filterAllocations(target, func(a allocationRecord) bool {
		return a.RolloutState == allocationRolloutServing && allocationReady(a)
	})
	if int32(len(servingTarget)) >= desiredTargetCount && len(predecessors) == 0 {
		if err := s.completeRolloutTx(ctx, tx, service, rollout, now); err != nil {
			return result, err
		}
		result.Changed = true
		result.IngressChanged = true
		return result, nil
	}

	missing := int(desiredTargetCount) - len(target)
	created := 0
	occupying := filterAllocations(allocs, allocationOccupiesRolloutSlot)
	if missing > 0 {
		limit := int(rollout.DesiredReplicaCount)
		if len(predecessors) > 0 {
			limit += int(rollout.Strategy.GetMaxSurge())
		}
		slots := limit - len(occupying)
		if slots > missing {
			slots = missing
		}
		if slots > 0 {
			occupied := make(map[string]struct{}, len(allocs))
			for _, alloc := range allocs {
				occupied[alloc.AgentID] = struct{}{}
			}
			for index := 0; index < slots; index++ {
				agentID, chooseErr := s.chooseReplicaAgentTx(ctx, tx, service, occupied, "", false)
				if errors.Is(chooseErr, errNoPlacementAvailable) {
					break
				}
				if chooseErr != nil {
					return result, chooseErr
				}
				alloc, insertErr := s.insertAllocationTx(ctx, tx, service, agentID, now)
				if insertErr != nil {
					return result, insertErr
				}
				occupied[agentID] = struct{}{}
				result.AgentIDs = append(result.AgentIDs, alloc.AgentID)
				result.Changed = true
				created++
			}
		}
	}

	// Withdraw old replicas when the strategy needs room (zero surge or
	// scale-down) without crossing maxUnavailable. Draining leftovers from a
	// previous attempt do not occupy surge slots.
	if !result.NeedsIngressConvergence {
		servingOld = filterAllocations(predecessors, func(a allocationRecord) bool {
			return a.RolloutState == allocationRolloutServing
		})
		available := len(servingTarget)
		for _, alloc := range servingOld {
			if allocationReady(alloc) {
				available++
			}
		}
		for _, alloc := range unaffected {
			if alloc.RolloutState == allocationRolloutServing && allocationReady(alloc) {
				available++
			}
		}
		minimumAvailable := int(rollout.DesiredReplicaCount - rollout.Strategy.GetMaxUnavailable())
		if minimumAvailable < 0 {
			minimumAvailable = 0
		}
		limit := int(rollout.DesiredReplicaCount)
		if len(predecessors) > 0 {
			limit += int(rollout.Strategy.GetMaxSurge())
		}
		over := len(filterAllocations(allocs, allocationOccupiesRolloutSlot)) + created - limit
		room := missing - created
		withdraw := available - minimumAvailable
		if withdraw < over {
			withdraw = over
		}
		if room > 0 && withdraw > room {
			withdraw = room
		}
		if withdraw > len(servingOld) {
			withdraw = len(servingOld)
		}
		if withdraw < 0 {
			withdraw = 0
		}
		sortAllocationsStable(servingOld)
		for index := 0; index < withdraw; index++ {
			message := "strategy allows temporary unavailability; waiting for ingress withdrawal"
			if over > 0 {
				message = "surplus predecessor; waiting for ingress withdrawal"
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations SET rollout_state = $1, phase = 'Withdrawing',
				        message = $2, updated_at = $3
				  WHERE id = $4`,
				allocationRolloutWithdrawing, message, now, servingOld[index].ID,
			); err != nil {
				return result, err
			}
			result.Changed = true
			result.IngressChanged = true
			result.NeedsIngressConvergence = true
		}
	}

	placement := ""
	if missing > 0 && created < missing && !result.NeedsIngressConvergence {
		placement = pendingPlacementMessage(len(target)+created, int(desiredTargetCount),
			"no eligible agent has capacity for the configured surge")
	}
	if err := s.setServicePlacementMessageTx(ctx, tx, serviceID, placement, now); err != nil {
		return result, err
	}

	if missing > 0 && created == 0 && !result.NeedsIngressConvergence &&
		now.Sub(rollout.ProgressAt) >= time.Duration(rollout.Strategy.GetStartupTimeoutSeconds())*time.Second {
		reason := fmt.Sprintf("could not schedule a replacement within %s: no eligible agent has capacity for the configured surge",
			time.Duration(rollout.Strategy.GetStartupTimeoutSeconds())*time.Second)
		if err := s.failRolloutTx(ctx, tx, service, rollout, target, reason, now); err != nil {
			return result, err
		}
		result.Changed = true
		result.AgentIDs = appendAllocationAgentIDs(result.AgentIDs, target...)
	}

	if result.Changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE service_rollouts SET progress_at = $1 WHERE service_id = $2 AND rollout_generation = $3`,
			now, serviceID, rollout.Generation,
		); err != nil {
			return result, err
		}
	}
	if err := s.updateRolloutProgressDetailTx(ctx, tx, serviceID, rollout, now); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) confirmRolloutIngressConverged(ctx context.Context, serviceID string, now time.Time) (rolloutAdvanceResult, error) {
	result := rolloutAdvanceResult{}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockServiceTx(ctx, tx, serviceID); err != nil {
			return err
		}
		service, err := s.serviceByIDInternalQuerier(ctx, tx, serviceID)
		if err != nil {
			return err
		}
		result.EnvironmentID = service.EnvironmentID
		rollout, ok, err := loadCurrentRolloutTx(ctx, tx, service)
		if err != nil {
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
		deadline := now.UTC().Add(time.Duration(strategy.GetDrainTimeoutSeconds()) * time.Second)
		rows, err := tx.QueryContext(ctx,
			`SELECT id, agent_id FROM allocations
			  WHERE service_id = $1 AND rollout_state = $2
			  ORDER BY created_at, id FOR UPDATE`,
			serviceID, allocationRolloutWithdrawing,
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
			if _, err := tx.ExecContext(ctx,
				`UPDATE allocations
				    SET rollout_state = $1, healthy = FALSE, healthy_ports = $2,
				        phase = 'Draining', message = 'ingress converged; gracefully draining',
				        drain_started_at = $3, drain_deadline = $4, updated_at = $3
				  WHERE id = $5 AND rollout_state = $6`,
				allocationRolloutDraining, []byte("[]"), now.UTC(), deadline, alloc.id, allocationRolloutWithdrawing,
			); err != nil {
				return err
			}
			result.AgentIDs = append(result.AgentIDs, alloc.agentID)
		}
		if !removing {
			if err := s.markPredecessorDeploymentsDrainingTx(ctx, tx, serviceID, rollout.Generation, now.UTC()); err != nil {
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
		}
		result.Changed = true
		return s.bumpAllDesiredRevisionsTx(ctx, tx)
	})
	return result, err
}

func currentRemovalDeploymentTx(ctx context.Context, tx *sql.Tx, serviceID string) (string, bool, error) {
	var deploymentID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM deployments
		  WHERE service_id = $1 AND is_current = TRUE AND state = $2 AND reason_code = $3
		  LIMIT 1 FOR UPDATE`,
		serviceID, deploymentStateDraining, reasonUserRemove,
	).Scan(&deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return deploymentID, err == nil, err
}

func loadCurrentRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord) (rolloutRecord, bool, error) {
	var rec rolloutRecord
	var strategyRaw []byte
	err := tx.QueryRowContext(ctx,
		`SELECT service_id, rollout_generation, spec_revision, state, strategy_json,
		        desired_replica_count, image_digest, failure_reason, target_allocation_id, created_at, progress_at
		   FROM service_rollouts
		  WHERE service_id = $1 AND rollout_generation = $2
		  FOR UPDATE`, service.ID, service.RolloutGeneration,
	).Scan(&rec.ServiceID, &rec.Generation, &rec.SpecRevision, &rec.State, &strategyRaw,
		&rec.DesiredReplicaCount, &rec.ImageDigest, &rec.FailureReason, &rec.TargetAllocationID, &rec.CreatedAt, &rec.ProgressAt)
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

func rolloutAllocationFailure(alloc allocationRecord, now time.Time, strategy *platformv1.RollingStrategy) string {
	switch alloc.Phase {
	case "Error", "Failed", "Unhealthy", "Stopped", "CrashLoop":
		return fmt.Sprintf("replacement allocation %s failed readiness: %s", alloc.ID, firstNonEmpty(alloc.Message, alloc.Phase))
	}
	if alloc.Restart.GetCrashLoop() || alloc.Phase == "CrashLoop" {
		return fmt.Sprintf("replacement allocation %s entered a crash loop: %s", alloc.ID, firstNonEmpty(alloc.Message, "restart budget exhausted"))
	}
	deadline := alloc.CreatedAt.Add(time.Duration(strategy.GetStartupTimeoutSeconds()) * time.Second)
	if !now.Before(deadline) {
		return fmt.Sprintf("replacement allocation %s did not become ready within %s: %s", alloc.ID,
			time.Duration(strategy.GetStartupTimeoutSeconds())*time.Second,
			firstNonEmpty(alloc.Message, "readiness check did not pass"))
	}
	return ""
}

func (s *Store) failRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, rollout rolloutRecord, target []allocationRecord, reason string, now time.Time) error {
	deadline := now.Add(time.Duration(rollout.Strategy.GetDrainTimeoutSeconds()) * time.Second)
	for _, alloc := range target {
		// A partially successful multi-replica rollout may already have healthy
		// target-generation allocations serving. Keep those alongside any healthy
		// predecessor; only never-routed replacements are cleanup candidates.
		if alloc.RolloutState != allocationRolloutStarting {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE allocations SET rollout_state = $1, healthy = FALSE, healthy_ports = $2,
			        phase = 'Draining', message = $3, drain_started_at = $4, drain_deadline = $5, updated_at = $4
			  WHERE id = $6`,
			allocationRolloutDraining, []byte("[]"), "failed replacement; cleaning up", now, deadline, alloc.ID,
		); err != nil {
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
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState: deploymentStateFailed, Actor: deploymentActor{Kind: deploymentCauseSystem},
		ReasonCode: reasonDeploymentFailed, Detail: reason,
	})
	return err
}

func (s *Store) completeRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, rollout rolloutRecord, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts SET state = $1, failure_reason = '', completed_at = $2, progress_at = $2
		  WHERE service_id = $3 AND rollout_generation = $4`,
		rolloutStateSucceeded, now, service.ID, rollout.Generation,
	); err != nil {
		return err
	}
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	if _, err := s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState: deploymentStateActive, Actor: deploymentActor{Kind: deploymentCauseSystem},
		ReasonCode: reasonDeploymentActive,
		Detail:     fmt.Sprintf("Rollout complete: %d of %d replacement replicas ready", rolloutTargetReplicaCount(rollout), rolloutTargetReplicaCount(rollout)),
	}); err != nil {
		return err
	}
	if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
		return err
	}
	return s.completeDrainedPredecessorsTx(ctx, tx, service.ID, dep.ID, deploymentActor{Kind: deploymentCauseSystem})
}

func (s *Store) markPredecessorDeploymentsDrainingTx(ctx context.Context, tx *sql.Tx, serviceID string, generation int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM deployments WHERE service_id = $1 AND rollout_generation < $2 AND state = $3 FOR UPDATE`,
		serviceID, generation, deploymentStateActive)
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
			ToState: deploymentStateDraining, Actor: deploymentActor{Kind: deploymentCauseSystem},
			ReasonCode: reasonDeploymentDraining, Detail: "Healthy replacements entered ingress; predecessor is draining",
		}); err != nil {
			return err
		}
	}
	_ = now
	return nil
}

func (s *Store) updateRolloutProgressDetailTx(ctx context.Context, tx *sql.Tx, serviceID string, rollout rolloutRecord, now time.Time) error {
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
	_, err := tx.ExecContext(ctx,
		`UPDATE deployments SET detail = $1, updated_at = $2
		  WHERE service_id = $3 AND rollout_generation = $4 AND is_current = TRUE AND state NOT IN ('failed','active')`,
		detail, now, serviceID, rollout.Generation)
	return err
}

func rolloutTargetReplicaCount(rollout rolloutRecord) int32 {
	if rollout.TargetAllocationID != "" {
		return 1
	}
	return rollout.DesiredReplicaCount
}

func filterAllocations(input []allocationRecord, keep func(allocationRecord) bool) []allocationRecord {
	out := make([]allocationRecord, 0, len(input))
	for _, alloc := range input {
		if keep(alloc) {
			out = append(out, alloc)
		}
	}
	return out
}

func sortAllocationsStable(input []allocationRecord) {
	sort.Slice(input, func(i, j int) bool {
		if !input[i].CreatedAt.Equal(input[j].CreatedAt) {
			return input[i].CreatedAt.Before(input[j].CreatedAt)
		}
		return input[i].ID < input[j].ID
	})
}

func appendAllocationAgentIDs(ids []string, allocations ...allocationRecord) []string {
	for _, alloc := range allocations {
		if strings.TrimSpace(alloc.AgentID) != "" {
			ids = append(ids, alloc.AgentID)
		}
	}
	return ids
}

func allocationOccupiesRolloutSlot(alloc allocationRecord) bool {
	switch alloc.RolloutState {
	case allocationRolloutStarting, allocationRolloutServing, allocationRolloutWithdrawing:
		return true
	default:
		return false
	}
}

func splitRolloutAllocations(allocs []allocationRecord, rollout rolloutRecord) (target, predecessors, unaffected []allocationRecord) {
	for _, alloc := range allocs {
		if alloc.RolloutState == allocationRolloutLost {
			continue
		}
		if alloc.DesiredRolloutGeneration == rollout.Generation {
			target = append(target, alloc)
			continue
		}
		if alloc.DesiredRolloutGeneration < rollout.Generation {
			if rollout.TargetAllocationID == "" || alloc.ID == rollout.TargetAllocationID {
				predecessors = append(predecessors, alloc)
			} else {
				unaffected = append(unaffected, alloc)
			}
		}
	}
	return target, predecessors, unaffected
}

func (s *Store) prepareReplacementRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, existing []allocationRecord, now time.Time) (bool, error) {
	if err := s.lockServiceTx(ctx, tx, service.ID); err != nil {
		return false, err
	}
	if serviceVolumeName(service.Spec) != "" && len(existing) > 0 {
		return false, errVolumeRollingUnsupported
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
	for _, alloc := range existing {
		if alloc.RolloutState == allocationRolloutServing {
			return false, errRolloutInProgress
		}
	}
	return true, s.supersedeUnservedRolloutTx(ctx, tx, service, rollout, existing, now)
}

func (s *Store) supersedeUnservedRolloutTx(ctx context.Context, tx *sql.Tx, service serviceRecord, rollout rolloutRecord, existing []allocationRecord, now time.Time) error {
	for _, alloc := range existing {
		if alloc.RolloutState == allocationRolloutLost {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM allocations WHERE id = $1`, alloc.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE service_rollouts SET state = $1, failure_reason = $2, completed_at = $3, progress_at = $3
		  WHERE service_id = $4 AND rollout_generation = $5`,
		rolloutStateSuperseded, "superseded by a newer rollout before any replica served traffic", now, service.ID, rollout.Generation,
	); err != nil {
		return err
	}
	dep, ok, err := s.deploymentByRolloutTx(ctx, tx, service.ID, rollout.Generation)
	if err != nil || !ok {
		return err
	}
	_, err = s.applyDeploymentTransitionTx(ctx, tx, dep.ID, deploymentTransitionInput{
		ToState:    deploymentStateSuperseded,
		Actor:      deploymentActor{Kind: deploymentCauseSystem},
		ReasonCode: reasonDeploymentSuperseded,
		Detail:     "Superseded before any replica became ready",
	})
	return err
}
