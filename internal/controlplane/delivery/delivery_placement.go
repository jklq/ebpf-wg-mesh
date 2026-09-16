package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func (d *Delivery) reconcileServiceReplicasTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, preferredAgentID string, now time.Time) ([]AllocationRecord, error) {
	s := d.store
	desired := service.DesiredReplicaCount
	if err := validateVolumeReplicaCompatibility(service.Spec, desired); err != nil {
		return nil, err
	}
	if desired <= 0 && service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return nil, nil
	}
	existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	if service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return existing, nil
	}

	live, lost := splitLostAllocations(existing)
	existing = live

	if int32(len(existing)) > desired {
		removed, remaining := selectAllocationsToRemove(existing, int(desired))
		for _, alloc := range removed {
			if err := d.applyAllocationMutationsTx(ctx, tx, now, AllocationMutation{Kind: MutationCompleteDrain, AllocationID: alloc.ID}); err != nil {
				return nil, err
			}
		}
		existing = remaining
	}

	if int32(len(existing)) < desired {
		needed := int(desired) - len(existing)
		occupied := make(map[string]struct{}, len(existing))
		for _, alloc := range existing {
			occupied[alloc.AgentID] = struct{}{}
		}
		placed := 0
		failureReason := "no active healthy node satisfies the placement constraints"
		for i := 0; i < needed; i++ {
			agentID, err := d.chooseReplicaAgentTx(ctx, tx, service, occupied, preferredAgentID, i == 0 && preferredAgentID != "")
			if errors.Is(err, ErrNoPlacementAvailable) {
				failureReason = strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrNoPlacementAvailable.Error()+": "))
				break
			}
			if err != nil {
				return nil, err
			}
			alloc, err := d.insertAllocationTx(ctx, tx, service, agentID, now)
			if err != nil {
				return nil, err
			}
			existing = append(existing, alloc)
			occupied[agentID] = struct{}{}
			placed++
			preferredAgentID = ""
		}
		if placed < needed {
			message := pendingPlacementMessage(len(existing), int(desired), failureReason)
			if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, message, now); err != nil {
				return nil, err
			}
			service.PlacementMessage = message
			return append(existing, lost...), nil
		}
	}

	if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
		return nil, err
	}
	service.PlacementMessage = ""
	return append(existing, lost...), nil
}

func (d *Delivery) chooseReplicaAgentTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, occupied map[string]struct{}, preferredAgentID string, usePreferred bool) (string, error) {
	s := d.store
	if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
		if err := s.requireVolumeQuerier(ctx, tx, service.EnvironmentID, volumeName); err != nil {
			return "", err
		}
	}
	if usePreferred && preferredAgentID != "" {
		if _, taken := occupied[preferredAgentID]; !taken {
			candidates, err := s.placementCandidatesQuerier(ctx, tx)
			if err != nil {
				return "", err
			}
			for _, candidate := range candidates {
				if candidate.ID == preferredAgentID && candidateEligible(candidate, service.Spec) {
					return preferredAgentID, nil
				}
			}
		}
	}
	return d.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied)
}

func (d *Delivery) chooseAgentForReplicaQuerier(ctx context.Context, q ServiceQueryer, spec *platformv1.ServiceSpec, occupied map[string]struct{}) (string, error) {
	s := d.store
	candidates, err := s.placementCandidatesQuerier(ctx, q)
	if err != nil {
		return "", err
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, true); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, false); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, false, false); agentID != "" {
		return agentID, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoPlacementAvailable, placementFailureReason(candidates, spec, s.reservedAgentIDs))
}
