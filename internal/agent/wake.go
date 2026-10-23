package agent

import (
	"net/netip"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/metering"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetWakeSource installs the mesh wake-counter source read from the
// firewall's pinned eBPF map. Without it, private-network wake is inactive
// and sleeping services only wake on public or operator triggers.
func (r *ContainerdRuntime) SetWakeSource(source metering.WakeSource) {
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	r.wakeSource = source
}

// sleepingAddresses maps normalized destination IPs of SLEEP-intent services
// to their allocation IDs.
func sleepingAddresses(services []*agentv1.DesiredService) map[[16]byte]string {
	out := make(map[[16]byte]string)
	for _, svc := range services {
		if svc.GetIntent() != agentv1.AllocationIntent_ALLOCATION_INTENT_SLEEP {
			continue
		}
		for _, raw := range []string{svc.GetPrivateIpv4(), svc.GetPrivateIpv6()} {
			addr, err := netip.ParseAddr(raw)
			if err != nil {
				continue
			}
			out[addr.As16()] = svc.GetAllocationId()
		}
	}
	return out
}

// sleepingImages collects the resolved images of SLEEP-intent services so
// the engine keeps them pinned against garbage collection.
func sleepingImages(services []*agentv1.DesiredService) map[string]bool {
	out := make(map[string]bool)
	for _, svc := range services {
		if svc.GetIntent() != agentv1.AllocationIntent_ALLOCATION_INTENT_SLEEP {
			continue
		}
		if image := svc.GetSpec().GetImage(); image != "" {
			out[image] = true
		}
	}
	return out
}

func (r *ContainerdRuntime) readWakeSamples() map[[16]byte]uint64 {
	r.wakeMu.Lock()
	source := r.wakeSource
	r.wakeMu.Unlock()
	if source == nil {
		return nil
	}
	samples, err := source.ReadWake()
	if err != nil {
		return nil
	}
	return samples
}

// CheckWakeDemand reports whether any sleeping allocation received mesh
// traffic since the last reconcile consumed the counters. The supervisor
// polls it to wake promptly instead of waiting for the safety resync.
func (r *ContainerdRuntime) CheckWakeDemand() bool {
	samples := r.readWakeSamples()
	if len(samples) == 0 {
		return false
	}
	r.wakeMu.Lock()
	defer r.wakeMu.Unlock()
	for ip := range r.sleepingAddrs {
		if samples[ip] > r.wakeBaseline[ip] {
			return true
		}
	}
	return false
}

// collectWakeDemands diffs the wake counters against the last reconcile and
// returns one demand per sleeping allocation that received traffic. The
// baseline advances here, in reconcile order, so a demand is reported
// exactly once per burst; packets keep arriving (and re-report) until the
// service actually wakes.
func (r *ContainerdRuntime) collectWakeDemands(services []*agentv1.DesiredService) []*agentv1.WakeDemand {
	sleeping := sleepingAddresses(services)
	r.wakeMu.Lock()
	r.sleepingAddrs = sleeping
	if r.wakeBaseline == nil {
		r.wakeBaseline = make(map[[16]byte]uint64)
	}
	r.wakeMu.Unlock()
	if len(sleeping) == 0 {
		return nil
	}
	samples := r.readWakeSamples()
	if len(samples) == 0 {
		return nil
	}
	now := timestamppb.New(time.Now().UTC())
	perAllocation := make(map[string]uint64)
	r.wakeMu.Lock()
	for ip, total := range samples {
		previous := r.wakeBaseline[ip]
		r.wakeBaseline[ip] = total
		if total <= previous {
			continue
		}
		if allocationID, ok := sleeping[ip]; ok {
			perAllocation[allocationID] += total - previous
		}
	}
	r.wakeMu.Unlock()
	demands := make([]*agentv1.WakeDemand, 0, len(perAllocation))
	for allocationID, packets := range perAllocation {
		demands = append(demands, &agentv1.WakeDemand{
			AllocationId: allocationID, Packets: packets, LastPacketAt: now,
		})
	}
	return demands
}
