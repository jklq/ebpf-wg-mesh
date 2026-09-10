package controlplane

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/routing"
	"ebof-wg-mesh/internal/restartpolicy"
)

// ingressLiveReader combines durable assignments with current session observations.
// Publication must be enabled before these backends can be published.
type ingressLiveReader interface {
	Publishing() bool
	Durable() journal.DurableState
	OverlayAllocation(deliverycore.AllocationRecord) deliverycore.AllocationRecord
}

func (s *routingPersistence) HealthyIngressBackends(ctx context.Context) ([]routing.Backend, error) {
	_ = ctx
	live := s.live
	if live == nil || !live.Publishing() {
		return nil, nil
	}
	return healthyIngressBackends(live.Durable(), live), nil
}

func healthyIngressBackends(durable journal.DurableState, live ingressLiveReader) []routing.Backend {
	type row struct {
		hostname, allocationID, ipv4, ipv6 string
		port                               int32
		ipv4Ports, ipv6Ports               []int32
	}
	var rows []row
	for _, domain := range durable.Domains {
		for _, assignment := range durable.Assignments {
			if assignment.ServiceID != domain.ServiceID {
				continue
			}
			rec := live.OverlayAllocation(deliverycore.AllocationRecord{
				ID: assignment.ID, ServiceID: assignment.ServiceID, AgentID: assignment.AgentID,
				DesiredSpecRevision: assignment.DesiredSpecRevision, DesiredRolloutGeneration: assignment.DesiredRolloutGeneration,
				AllocationIPv4: assignment.AllocationIPv4, AllocationIPv6: assignment.AllocationIPv6,
				RolloutState: assignment.RolloutState, Message: assignment.IntentMessage,
			})
			if !rec.Healthy || rec.RolloutState != deliverycore.AllocationRolloutServing ||
				rec.AppliedSpecRevision < rec.DesiredSpecRevision || rec.AppliedRolloutGeneration < rec.DesiredRolloutGeneration ||
				rec.Phase == restartpolicy.PhaseCrashLoop {
				continue
			}
			rows = append(rows, row{
				hostname: domain.Hostname, allocationID: rec.ID, ipv4: rec.AllocationIPv4, ipv6: rec.AllocationIPv6,
				port: int32(domain.TargetPort), ipv4Ports: rec.HealthyIPv4Ports, ipv6Ports: rec.HealthyIPv6Ports,
			})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		if n := strings.Compare(a.hostname, b.hostname); n != 0 {
			return n
		}
		return strings.Compare(a.allocationID, b.allocationID)
	})
	var backends []routing.Backend
	for _, item := range rows {
		if slices.Contains(item.ipv4Ports, item.port) && net.ParseIP(item.ipv4) != nil {
			backends = append(backends, routing.Backend{
				Domain: item.hostname, Upstream: net.JoinHostPort(item.ipv4, strconv.Itoa(int(item.port))), AllocationID: item.allocationID,
			})
		} else if slices.Contains(item.ipv6Ports, item.port) && net.ParseIP(item.ipv6) != nil {
			backends = append(backends, routing.Backend{
				Domain: item.hostname, Upstream: net.JoinHostPort(item.ipv6, strconv.Itoa(int(item.port))), AllocationID: item.allocationID,
			})
		}
	}
	return backends
}
