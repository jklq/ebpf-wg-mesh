package reconciliation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrIdentityRecovery = errors.New("identity recovery required")

const (
	GrantLifetime = 15 * time.Second
	// Agents must keep their wall clock within this bound of database time.
	// Suspend/resume must not stop that clock. Uncertain clocks must fail closed.
	MaxClockSkew = time.Second
)

// FencedCommand is any agent-scoped control-plane message carrying session
// and authority-expiry fencing: checkpoints, diffs, and the independently
// versioned node-config, credential, and replica messages.
type FencedCommand interface {
	GetSessionId() string
	GetAuthorityNotAfter() *timestamppb.Timestamp
}

func ValidateCommand(state FencedCommand, sessionID string, now time.Time) error {
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

// HashNodeConfig versions node config content for independent delivery.
// Shared by control plane and agent so both compute the same version.
func HashNodeConfig(config *agentv1.AssignedNodeConfig) string {
	if config == nil {
		return hashBytes([]byte("node-config:nil"))
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(config)
	if err != nil {
		return ""
	}
	return hashBytes(raw)
}

// HashCredentials versions pull credentials content.
func HashCredentials(creds []*agentv1.AllocationCredential) string {
	ordered := append([]*agentv1.AllocationCredential(nil), creds...)
	slices.SortFunc(ordered, func(a, b *agentv1.AllocationCredential) int {
		return strings.Compare(a.GetAllocationId(), b.GetAllocationId())
	})
	h := sha256.New()
	for _, c := range ordered {
		h.Write([]byte(c.GetAllocationId()))
		h.Write([]byte{0})
		h.Write([]byte(c.GetUsername()))
		h.Write([]byte{0})
		h.Write([]byte(c.GetPassword()))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashReplicas versions replica discovery content.
func HashReplicas(addresses []string) string {
	ordered := append([]string(nil), addresses...)
	slices.Sort(ordered)
	h := sha256.New()
	for _, addr := range ordered {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		h.Write([]byte(addr))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
