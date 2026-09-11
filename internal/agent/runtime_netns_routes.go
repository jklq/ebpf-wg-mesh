package agent

import (
	"fmt"
	"net"
)

// decideWorkloadPoolRoute is the fail-closed policy for rewriting a workload
// family's on-link pool via its default gateway. A default without a pool, a
// pool without a default, or routes on different links are errors so the
// workload cannot start with cross-agent routing still broken.
func decideWorkloadPoolRoute(gateway net.IP, pool *net.IPNet, gatewayLink, poolLink int) error {
	if gateway == nil && pool == nil {
		return nil
	}
	if gateway == nil || pool == nil {
		poolName := "<nil>"
		if pool != nil {
			poolName = pool.String()
		}
		return fmt.Errorf("workload route family incomplete: gateway=%v pool=%s", gateway, poolName)
	}
	if gatewayLink == 0 || poolLink == 0 || gatewayLink != poolLink {
		return fmt.Errorf("workload gateway and pool use different links: gateway=%d pool=%d", gatewayLink, poolLink)
	}
	return nil
}
