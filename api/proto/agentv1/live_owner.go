package agentv1

import (
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const LiveOwnerRedirectPrefix = "not the live owner; reconnect at "

// Reasons a control-plane replica ends or refuses a session on purpose. The
// replica is healthy in each case: only a draining replica should be avoided.
const (
	ReasonReplicaDraining = "REPLICA_DRAINING"
	ReasonOwnerNotReady   = "LIVE_OWNER_NOT_READY"
	ReasonOwnerMoved      = "LIVE_OWNER_MOVED"
	reconnectDomain       = "ebpf-wg-mesh.controlplane"
)

// OwnerHandoverDelay exceeds a peer's lease acquisition interval, so a client
// told that no owner is ready usually finds the new owner on its first retry.
const OwnerHandoverDelay = 1500 * time.Millisecond

func LiveOwnerRedirect(address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return status.Error(codes.Unavailable, "control plane has no reachable live owner")
	}
	return status.Error(codes.FailedPrecondition, LiveOwnerRedirectPrefix+address)
}

// ReconnectSpread paces clients that one replica disconnects together, so its
// peers absorb about 250 new sessions per second for at most ten seconds. A few
// clients reconnect immediately.
func ReconnectSpread(clients int) time.Duration {
	return min(10*time.Second, time.Duration(max(clients, 0))*4*time.Millisecond)
}

// ReconnectDelay is base plus a uniformly random share of the spread.
func ReconnectDelay(base time.Duration, clients int) time.Duration {
	if spread := ReconnectSpread(clients); spread > 0 {
		base += rand.N(spread)
	}
	return base
}

// WithReconnectGuidance tells the client why its session ended and when to
// reconnect. Clients that ignore error details keep their own backoff.
func WithReconnectGuidance(err error, reason string, after time.Duration) error {
	st := status.Convert(err)
	detailed, detailErr := st.WithDetails(
		&errdetails.ErrorInfo{Reason: reason, Domain: reconnectDomain},
		&errdetails.RetryInfo{RetryDelay: durationpb.New(after)},
	)
	if detailErr != nil {
		return err
	}
	return detailed.Err()
}

// ReconnectGuidance reports the reason and delay attached by a replica.
func ReconnectGuidance(err error) (reason string, after time.Duration, ok bool) {
	st, isStatus := status.FromError(err)
	if !isStatus {
		return "", 0, false
	}
	for _, detail := range st.Details() {
		switch d := detail.(type) {
		case *errdetails.ErrorInfo:
			if d.GetDomain() == reconnectDomain {
				reason, ok = d.GetReason(), true
			}
		case *errdetails.RetryInfo:
			after = d.GetRetryDelay().AsDuration()
		}
	}
	return reason, max(after, 0), ok
}
