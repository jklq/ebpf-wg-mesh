package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/testutil"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stressLease struct {
	Holder  string
	Address string
	Token   int64
}

func readStressLease(ctx context.Context, key, host string) (stressLease, error) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runRemoteCommand(readCtx, key, host, "cockroach sql --insecure --host=127.0.0.1:26257 --format=tsv --execute \"SELECT holder_id, fencing_token, advertise_addr FROM control_plane_leases WHERE name = 'control-plane-singleton' AND expires_at > statement_timestamp();\"")
	if err != nil {
		return stressLease{}, fmt.Errorf("read singleton lease: %w", err)
	}
	return parseStressLease(string(out))
}

func parseStressLease(output string) (stressLease, error) {
	var leases []stressLease
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] == "holder_id" {
			continue
		}
		var token int64
		if _, err := fmt.Sscanf(fields[1], "%d", &token); err != nil {
			continue
		}
		leases = append(leases, stressLease{Holder: fields[0], Token: token, Address: fields[2]})
	}
	if len(leases) != 1 {
		return stressLease{}, fmt.Errorf("expected exactly one live singleton lease, found %d", len(leases))
	}
	return leases[0], nil
}

// waitForStressLease polls until exactly one live lease is held by a known
// replica. Zero during an election is transient.
func waitForStressLease(ctx context.Context, key, controlPlaneIP string) (stressLease, error) {
	known := map[string]bool{
		controlPlaneIP + ":" + primaryControlPlanePort: true,
		controlPlaneIP + ":" + replicaControlPlanePort: true,
	}
	var current stressLease
	var lastErr error
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: 2 * time.Minute, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		lease, err := readStressLease(ctx, key, controlPlaneIP)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if !known[lease.Address] {
			return false, fmt.Errorf("singleton lease advertised by unknown replica address %q", lease.Address)
		}
		current = lease
		return true, nil
	})
	if err != nil {
		return stressLease{}, errors.Join(err, lastErr)
	}
	return current, nil
}

var errNoOwnerServing = errors.New("no replica served owner-local RPCs")

// checkSingleOwner compares owner-gated RPC results to the post-probe lease,
// the only owner identity; a failover between snapshot and check must not
// fail. Split brain is always a hard failure.
func checkSingleOwner(owners []string, current, baseline stressLease) error {
	if current.Token < baseline.Token {
		return fmt.Errorf("fencing token regressed from %d to %d (holder %s)", baseline.Token, current.Token, current.Holder)
	}
	if len(owners) > 1 {
		return fmt.Errorf("split brain: replicas %v all served owner-local RPCs with lease holder %s", owners, current.Holder)
	}
	if len(owners) == 0 {
		return fmt.Errorf("%w while lease holder is %s", errNoOwnerServing, current.Holder)
	}
	if owners[0] != current.Address {
		return fmt.Errorf("owner serving RPCs (%s) does not match lease holder (%s)", owners[0], current.Address)
	}
	return nil
}

// assertSingleOwner calls an owner-gated RPC on both replicas. Exactly the
// leased replica may serve it; both serving is split brain. Neither serving
// is transient during an election and is retried.
func assertSingleOwner(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, key, controlPlaneIP string, lease stressLease) error {
	addresses := []string{controlPlaneIP + ":" + primaryControlPlanePort, controlPlaneIP + ":" + replicaControlPlanePort}
	return assertSingleOwnerWith(ctx, o, clients, addresses, lease, func(ctx context.Context) (stressLease, error) {
		return readStressLease(ctx, key, controlPlaneIP)
	})
}

func assertSingleOwnerWith(ctx context.Context, o stressOptions, clients []platformv1.PlatformServiceClient, addresses []string, baseline stressLease, readLease func(context.Context) (stressLease, error)) error {
	var lastErr error
	err := testutil.Poll(ctx, testutil.PollConfig{Timeout: o.Recovery, Interval: time.Second}, func(ctx context.Context) (bool, error) {
		// Probe first: judge against the post-probe lease, not a stale holder.
		var owners []string
		for i, client := range clients {
			callCtx, cancel := context.WithTimeout(ctx, o.RPCTimeout)
			_, err := client.ListAgents(callCtx, &emptypb.Empty{})
			cancel()
			switch status.Code(err) {
			case codes.OK:
				owners = append(owners, addresses[i])
			case codes.FailedPrecondition:
				if _, ok := delivery.ParseLiveOwnerRedirect(status.Convert(err).Message()); !ok {
					return false, fmt.Errorf("ListAgents on %s returned non-redirect FailedPrecondition: %w", addresses[i], err)
				}
			case codes.Unavailable, codes.DeadlineExceeded:

			default:
				return false, fmt.Errorf("ListAgents on %s: %w", addresses[i], err)
			}
		}
		current, err := readLease(ctx)
		if err != nil {
			lastErr = err
			return false, nil
		}
		switch err := checkSingleOwner(owners, current, baseline); {
		case err == nil:
			return true, nil
		case errors.Is(err, errNoOwnerServing):
			lastErr = err
			return false, nil
		default:
			return false, err
		}
	})
	if err != nil {
		return errors.Join(err, lastErr)
	}
	return nil
}
