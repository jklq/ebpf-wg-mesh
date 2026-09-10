package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

var (
	errReplicaDial      = errors.New("control-plane replica dial failed")
	errReplicaRPC       = errors.New("control-plane replica rpc failed")
	errOwnerQuarantined = errors.New("live owner is quarantined")
	errHandshakeTimeout = errors.New("sync handshake timeout")
	replicaDialTimeout  = 5 * time.Second
	replicaRPCTimeout   = 5 * time.Second
	deadOwnerCooldown   = 10 * time.Second
)

func (a *App) controlPlaneCandidates() ([]string, error) {
	addresses := append([]string(nil), a.cfg.ControlPlane.Addresses...)
	if a.stateStore != nil {
		persisted, err := a.stateStore.replicaAddresses()
		if err != nil {
			return nil, err
		}
		addresses = appendUniqueAddresses(addresses, persisted...)
	}
	return discoveryCandidates(a.controlPlaneAddr, addresses, a.quarantinedOwners()), nil
}

func discoveryCandidates(pinned string, addresses []string, quarantined map[string]time.Time) []string {
	now := time.Now()
	isQuarantined := func(address string) bool {
		until, ok := quarantined[strings.TrimSpace(address)]
		return ok && now.Before(until)
	}
	ready := make([]string, 0, len(addresses)+1)
	pinned = strings.TrimSpace(pinned)
	if pinned != "" && !isQuarantined(pinned) {
		ready = append(ready, pinned)
	}
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" || isQuarantined(address) {
			continue
		}
		ready = appendUniqueAddresses(ready, address)
	}
	return ready
}

func forgetDeadPin(pinned, attempted string, err error) string {
	if strings.TrimSpace(pinned) == "" || strings.TrimSpace(pinned) != strings.TrimSpace(attempted) || !isReplicaRetryable(err) {
		return pinned
	}
	return ""
}

func (a *App) quarantinedOwners() map[string]time.Time {
	if a == nil || len(a.deadOwners) == 0 {
		return nil
	}
	now := time.Now()
	out := make(map[string]time.Time, len(a.deadOwners))
	for address, until := range a.deadOwners {
		if now.Before(until) {
			out[address] = until
		}
	}
	return out
}

func (a *App) ownerQuarantined(address string) bool {
	address = strings.TrimSpace(address)
	if a == nil || address == "" || len(a.deadOwners) == 0 {
		return false
	}
	until, ok := a.deadOwners[address]
	return ok && time.Now().Before(until)
}

func (a *App) quarantineOwner(address string) {
	address = strings.TrimSpace(address)
	if a == nil || address == "" {
		return
	}
	if a.deadOwners == nil {
		a.deadOwners = make(map[string]time.Time)
	}
	if until, ok := a.deadOwners[address]; ok && time.Now().Before(until) {
		return
	}
	a.deadOwners[address] = time.Now().Add(deadOwnerCooldown)
}

func (a *App) markReplicaUnavailable(address string, err error) {
	if !isReplicaRetryable(err) {
		return
	}
	a.controlPlaneAddr = forgetDeadPin(a.controlPlaneAddr, address, err)
	a.quarantineOwner(address)
}

func (a *App) followLiveOwner(err error) error {
	owner, ok := liveOwnerAddr(err)
	if !ok {
		return nil
	}
	if a.ownerQuarantined(owner) {
		return fmt.Errorf("%w: %w", errReplicaRPC, errOwnerQuarantined)
	}
	a.controlPlaneAddr = owner
	return errRedirectOwner
}

func (a *App) replicaAttemptError(address string, err error) error {
	if err == nil || errors.Is(err, errRedirectOwner) || errors.Is(err, errRotateSession) {
		return err
	}
	if redirectErr := a.followLiveOwner(err); redirectErr != nil {
		return redirectErr
	}
	if !isReplicaRetryable(err) {
		return err
	}
	a.markReplicaUnavailable(address, err)
	return controlPlaneUnavailableError(address, err)
}

func shouldWalkNextReplica(err error) bool {
	return isReplicaRetryable(err)
}

func dialControlPlane(_ context.Context, address string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReplicaDial, err)
	}
	return conn, nil
}

func replicaRPCContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, replicaRPCTimeout)
}

func handshakeCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errRotateSession):
		return errRotateSession
	case errors.Is(cause, errHandshakeTimeout):
		return fmt.Errorf("%w: %w", errReplicaRPC, cause)
	default:
		return nil
	}
}

func isReplicaRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errRotateSession) {
		return false
	}
	if errors.Is(err, errReplicaDial) || errors.Is(err, errReplicaRPC) || errors.Is(err, errOwnerQuarantined) || errors.Is(err, errHandshakeTimeout) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	switch grpcStatusCode(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

type grpcStatuser interface {
	GRPCStatus() *status.Status
}

func grpcStatusCode(err error) codes.Code {
	if st, ok := status.FromError(err); ok {
		return st.Code()
	}
	var statuser grpcStatuser
	if errors.As(err, &statuser) && statuser.GRPCStatus() != nil {
		return statuser.GRPCStatus().Code()
	}
	return codes.Unknown
}

func liveOwnerAddr(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if st, ok := status.FromError(err); ok {
		if address, found := ownerAddrFromMessage(st.Message()); found {
			return address, true
		}
	}
	var statuser grpcStatuser
	if errors.As(err, &statuser) && statuser.GRPCStatus() != nil {
		if address, found := ownerAddrFromMessage(statuser.GRPCStatus().Message()); found {
			return address, true
		}
	}
	return ownerAddrFromMessage(err.Error())
}

func ownerAddrFromMessage(msg string) (string, bool) {
	idx := strings.Index(msg, agentv1.LiveOwnerRedirectPrefix)
	if idx < 0 {
		return "", false
	}
	address := strings.TrimSpace(msg[idx+len(agentv1.LiveOwnerRedirectPrefix):])
	if cut := strings.IndexAny(address, " \n\t"); cut >= 0 {
		address = strings.TrimSpace(address[:cut])
	}
	return address, address != ""
}

func controlPlaneUnavailableError(address string, err error) error {
	return fmt.Errorf("control plane replica %s unavailable: %w", address, err)
}
