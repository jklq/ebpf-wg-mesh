package delivery

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"
)

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
		// Missing local mesh access is not evidence of workload failure.
		if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			unknown = err
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, unknown
}

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
			// Takeover loses health evidence; retain routed work until failure or withdrawal is confirmed.
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
