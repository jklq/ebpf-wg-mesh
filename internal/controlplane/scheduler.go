package controlplane

import (
	"context"
	"errors"
	"sort"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

var errNoPlacementAvailable = errors.New("no healthy agent satisfies placement")

type Scheduler struct {
	store *Store
}

func NewScheduler(store *Store) *Scheduler {
	return &Scheduler{store: store}
}

func (s *Scheduler) ChooseAgentForVolume(ctx context.Context) (string, error) {
	return s.store.chooseAgentForVolume(ctx)
}

func (s *Scheduler) ChooseAgentForService(ctx context.Context, projectID string, spec *platformv1.ServiceSpec) (string, error) {
	return s.store.chooseAgentForService(ctx, projectID, spec)
}

func chooseAgent(agents []agentRecord, services []serviceRecord, spec *platformv1.ServiceSpec) (string, error) {
	now := time.Now().UTC()
	type candidate struct {
		id    string
		score int
	}
	usage := make(map[string]struct{ cpu, mem int64 })
	counts := make(map[string]int)
	for _, svc := range services {
		counts[svc.AllocatedAgentID]++
		if svc.Spec != nil {
			current := usage[svc.AllocatedAgentID]
			current.cpu += svc.Spec.CpuMillis
			current.mem += svc.Spec.MemoryMebibytes
			usage[svc.AllocatedAgentID] = current
		}
	}
	var candidates []candidate
	for _, agent := range agents {
		if !agent.healthy(now) {
			continue
		}
		use := usage[agent.ID]
		if spec != nil {
			if agent.CPUMillisCapacity > 0 && use.cpu+spec.CpuMillis > agent.CPUMillisCapacity {
				continue
			}
			if agent.MemoryMebibytesCapcity > 0 && use.mem+spec.MemoryMebibytes > agent.MemoryMebibytesCapcity {
				continue
			}
		}
		candidates = append(candidates, candidate{id: agent.ID, score: counts[agent.ID]})
	}
	if len(candidates) == 0 {
		return "", errNoPlacementAvailable
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].score < candidates[j].score
	})
	return candidates[0].id, nil
}
