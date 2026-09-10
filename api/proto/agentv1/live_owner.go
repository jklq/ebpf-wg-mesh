package agentv1

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LiveOwnerRedirectPrefix is the gRPC status message prefix used when a
// replica is not the singleton leaseholder. Agents parse the remainder as
// the address they should reconnect to.
const LiveOwnerRedirectPrefix = "not the live owner; reconnect at "

func LiveOwnerRedirect(address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return status.Error(codes.Unavailable, "control plane has no reachable live owner")
	}
	return status.Error(codes.FailedPrecondition, LiveOwnerRedirectPrefix+address)
}
