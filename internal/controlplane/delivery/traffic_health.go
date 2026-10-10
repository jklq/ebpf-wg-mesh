package delivery

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"
)

// Probes supplement stale management presence. A reachable listener is evidence
// that replacing its allocation would interrupt a working data plane. Envoy
// independently probes the same endpoints from the traffic-serving path.
func probeAllocationTraffic(ctx context.Context, allocation AllocationRecord) (bool, error) {
	var targets []string
	for _, family := range []struct {
		ip    string
		ports []int32
	}{
		{allocation.AllocationIPv4, allocation.HealthyIPv4Ports},
		{allocation.AllocationIPv6, allocation.HealthyIPv6Ports},
	} {
		if net.ParseIP(family.ip) == nil {
			continue
		}
		for _, port := range family.ports {
			targets = append(targets, net.JoinHostPort(family.ip, strconv.Itoa(int(port))))
		}
	}
	if len(targets) == 0 {
		return false, errors.New("allocation has no reported listener to probe")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	results := make(chan error, len(targets))
	for _, target := range targets {
		go func() {
			conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", target)
			if err == nil {
				_ = conn.Close()
			}
			results <- err
		}()
	}
	var unknown error
	for range targets {
		err := <-results
		if err == nil {
			return true, nil
		}
		// Missing local access to the mesh is not workload failure evidence.
		if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			unknown = err
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, unknown
}

// Keep probe failures outside the SQL transaction and bound fanout across a
// large agent. An inaccessible local mesh defers replacement just like success.
func (d *Delivery) allocationsRetainedByTraffic(ctx context.Context, agentID string) map[string]AllocationRecord {
	var candidates []AllocationRecord
	retained := make(map[string]AllocationRecord)
	product := d.live.Product()
	for _, allocation := range d.live.AllocationsByAgent(agentID) {
		if allocation.RolloutState != AllocationRolloutServing {
			continue
		}
		if allocation.Healthy {
			candidates = append(candidates, allocation)
		} else if _, observed := d.live.Observation(allocation.ID, allocation.DesiredRolloutGeneration); !observed && len(product.DomainHostnamesForService(allocation.ServiceID)) > 0 {
			// Takeover erased health evidence, not the running workload. Keep
			// routed allocations until an agent supplies failure evidence or
			// durable intent explicitly withdraws them.
			retained[allocation.ID] = allocation
		}
	}
	jobs := make(chan AllocationRecord, len(candidates))
	results := make(chan AllocationRecord, len(candidates))
	for _, allocation := range candidates {
		jobs <- allocation
	}
	close(jobs)
	for range min(8, len(candidates)) {
		go func() {
			for allocation := range jobs {
				ok, err := probeAllocationTraffic(ctx, allocation)
				if ok || err != nil {
					results <- allocation
				} else {
					results <- AllocationRecord{}
				}
			}
		}()
	}
	for range candidates {
		if allocation := <-results; allocation.ID != "" {
			retained[allocation.ID] = allocation
		}
	}
	return retained
}
