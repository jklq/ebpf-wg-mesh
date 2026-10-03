package delivery

import (
	"sort"
	"strings"

	"ebof-wg-mesh/internal/controlplane/journal"

	"google.golang.org/protobuf/proto"
)

// AffectedAgentIDs identifies agents whose desired state depends on a product change.
// Both projections are used so removal and movement notify former hosts too.
func AffectedAgentIDs(before, after *journal.Projection, batch journal.Batch) []string {
	environments, services, agents := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	add := func(set map[string]struct{}, id string) {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	serviceEnvironment := func(id string) {
		add(environments, before.Services[id].EnvironmentID)
		add(environments, after.Services[id].EnvironmentID)
	}
	for _, c := range batch.Environments {
		add(environments, c.Key)
	}
	// Volume rows render on their pinned agent; the environment's services
	// resolve mounts by volume name, so its hosts re-render too.
	for _, c := range batch.Volumes {
		add(environments, before.Volumes[c.Key].EnvironmentID)
		add(environments, after.Volumes[c.Key].EnvironmentID)
		add(agents, before.Volumes[c.Key].AgentID)
		add(agents, after.Volumes[c.Key].AgentID)
	}
	for _, c := range batch.Destructions {
		add(agents, before.Destructions[c.Key].AgentID)
		add(agents, after.Destructions[c.Key].AgentID)
	}
	for _, c := range batch.Services {
		serviceEnvironment(c.Key)
	}
	for _, c := range batch.Revisions {
		id, _, _ := strings.Cut(c.Key, "/")
		serviceEnvironment(id)
	}
	for _, c := range batch.Assignments {
		old, next := before.Assignments[c.Key], after.Assignments[c.Key]
		add(agents, old.AgentID)
		add(agents, next.AgentID)
		serviceEnvironment(old.ServiceID)
		serviceEnvironment(next.ServiceID)
	}
	for _, id := range peerChangedAgentIDs(before, after, batch) {
		for _, env := range before.EnvironmentIDsForAgent(id) {
			add(environments, env)
		}
		for _, env := range after.EnvironmentIDsForAgent(id) {
			add(environments, env)
		}
	}
	for _, id := range selfAgentIDs(before, after, batch) {
		add(agents, id)
	}
	for _, c := range batch.Rollouts {
		id, _, _ := strings.Cut(c.Key, "/")
		add(services, id)
	}
	for _, c := range batch.Domains {
		add(services, before.Domains[c.Key].ServiceID)
		add(services, after.Domains[c.Key].ServiceID)
	}
	for _, c := range batch.Projects {
		for _, env := range before.EnvironmentIDsForProject(c.Key) {
			add(environments, env)
		}
		for _, env := range after.EnvironmentIDsForProject(c.Key) {
			add(environments, env)
		}
	}
	for env := range environments {
		for _, id := range before.AgentIDsForEnvironment(env) {
			add(agents, id)
		}
		for _, id := range after.AgentIDsForEnvironment(env) {
			add(agents, id)
		}
	}
	for service := range services {
		for _, id := range before.AssignmentIDsForService(service) {
			add(agents, before.Assignments[id].AgentID)
		}
		for _, id := range after.AssignmentIDsForService(service) {
			add(agents, after.Assignments[id].AgentID)
		}
	}
	return keysOf(agents)
}

func peerChangedAgentIDs(base, after *journal.Projection, batch journal.Batch) []string {
	var out []string
	for _, id := range changedAgentIDs(batch) {
		beforeAgent, hasBeforeAgent := base.Agents[id]
		beforePeer := renderWireGuardPeer(beforeAgent, base.Administration[id], 0)
		if !hasBeforeAgent {
			beforePeer = nil
		}
		afterAgent, hasAfterAgent := after.Agents[id]
		afterPeer := renderWireGuardPeer(afterAgent, after.Administration[id], 0)
		if !hasAfterAgent {
			afterPeer = nil
		}
		if !proto.Equal(beforePeer, afterPeer) {
			out = append(out, id)
		}
	}
	return out
}

func selfAgentIDs(base, after *journal.Projection, batch journal.Batch) []string {
	var out []string
	for _, id := range changedAgentIDs(batch) {
		afterAgent, hasAfter := after.Agents[id]
		if !hasAfter {
			continue
		}
		beforeAgent, hasBefore := base.Agents[id]
		if selfNodeConfigChanged(beforeAgent, hasBefore, afterAgent) {
			out = append(out, id)
		}
	}
	return out
}

func changedAgentIDs(batch journal.Batch) []string {
	ids := map[string]struct{}{}
	for _, change := range batch.Agents {
		ids[change.Key] = struct{}{}
	}
	for _, change := range batch.Administration {
		ids[change.Key] = struct{}{}
	}
	return keysOf(ids)
}

func selfNodeConfigChanged(before journal.AgentRegistration, hasBefore bool, after journal.AgentRegistration) bool {
	if !hasBefore {
		return after.WireguardIPv6 != "" || after.WireguardListenPort > 0
	}
	return before.WireguardIPv6 != after.WireguardIPv6 || before.WireguardListenPort != after.WireguardListenPort
}

func keysOf(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
