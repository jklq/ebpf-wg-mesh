//go:build !linux

package agent

import (
	"context"
	"net"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
)

func probeServiceHealthInNamespace(ctx context.Context, _ string, allocationIP string, svc *agentv1.DesiredService) serviceHealthProbe {
	return probeServiceHealthWithDialer(ctx, allocationIP, svc, (&net.Dialer{}).DialContext)
}
