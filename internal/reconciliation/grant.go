// Package reconciliation defines the transport authority contract shared by
// agents and the control plane. Workload ownership is a separate contract.
package reconciliation

import (
	"errors"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

var ErrIdentityRecovery = errors.New("identity recovery required")

const (
	GrantLifetime = 15 * time.Second
	// Agents must keep their wall clock within this bound of database time.
	// Suspend/resume must not stop that clock. Uncertain clocks must fail closed.
	MaxClockSkew = time.Second
)

// ValidateCommand fences new acceptance decisions. The agent must durably stage
// the exact candidate before its final check; committing that decision and
// reconciling already accepted state may finish after grant expiry.
func ValidateCommand(state *agentv1.DesiredNodeState, sessionID string, now time.Time) error {
	if sessionID == "" || state.GetSessionId() != sessionID {
		return errors.New("desired state belongs to another session")
	}
	deadline := state.GetAuthorityNotAfter()
	if deadline == nil || deadline.CheckValid() != nil || !now.Add(MaxClockSkew).Before(deadline.AsTime()) {
		return errors.New("desired state authority grant expired or missing")
	}
	if deadline.AsTime().After(now.Add(GrantLifetime + MaxClockSkew)) {
		return errors.New("desired state authority grant exceeds maximum lifetime")
	}
	return nil
}

// CanTakeOver uses database time and a deadline persisted when granting
// authority. Early release must never shorten that deadline.
func CanTakeOver(now, outstandingNotAfter time.Time) bool {
	return !now.Before(outstandingNotAfter)
}
