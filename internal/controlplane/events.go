package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
)

const (
	maxPlatformBlockingWait    = 5 * time.Minute
	defaultEventPollInterval   = 250 * time.Millisecond
	initialEnvironmentRevision = int64(1)
	globalEnvironmentEventID   = "__control_plane_global__"
)

type platformEvents struct {
	store        globalRevisionStore
	pollInterval time.Duration
}

type globalRevisionStore interface {
	currentGlobalRevision(context.Context) (int64, error)
}

func newPlatformEvents(store globalRevisionStore, pollInterval time.Duration) *platformEvents {
	if pollInterval <= 0 {
		pollInterval = defaultEventPollInterval
	}
	return &platformEvents{store: store, pollInterval: pollInterval}
}

func (e *platformEvents) Current(ctx context.Context) (int64, error) {
	if e == nil || e.store == nil {
		return initialEnvironmentRevision, nil
	}
	return e.store.currentGlobalRevision(ctx)
}

func (s *database) currentGlobalRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT revision FROM environment_events WHERE environment_id = $1`, globalEnvironmentEventID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return initialEnvironmentRevision, nil
	}
	return revision, err
}

func bumpGlobalEnvironmentEventTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_events(environment_id, revision, updated_at)
		VALUES ($1, $2, statement_timestamp())
		ON CONFLICT(environment_id) DO UPDATE
		SET revision = environment_events.revision + 1, updated_at = statement_timestamp()`,
		globalEnvironmentEventID, initialEnvironmentRevision+1)
	return err
}

func (e *platformEvents) Wait(ctx context.Context, after int64, timeout time.Duration) (int64, bool, error) {
	current, err := e.Current(ctx)
	if err != nil {
		return 0, false, err
	}
	if after <= 0 || current > after {
		return current, true, nil
	}
	if timeout <= 0 || timeout > maxPlatformBlockingWait {
		timeout = maxPlatformBlockingWait
	}
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	poll := time.NewTicker(e.pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return current, false, ctx.Err()
		case <-timeoutTimer.C:
			return current, false, nil
		case <-poll.C:
			current, err = e.Current(ctx)
			if err != nil {
				return 0, false, err
			}
			if current > after {
				return current, true, nil
			}
		}
	}
}

func platformWaitDuration(seconds int32) time.Duration {
	if seconds <= 0 {
		return maxPlatformBlockingWait
	}
	return time.Duration(seconds) * time.Second
}

func (s *database) bumpAffectedAgents(ctx context.Context, tx *sql.Tx, base *journal.Projection, batch journal.Batch) error {
	after, err := base.Preview(batch)
	if err != nil {
		return err
	}
	return dbtx.BumpDesiredRevisions(ctx, tx, affectedAgentIDs(base, after, batch))
}

func affectedAgentIDs(before, after *journal.Projection, batch journal.Batch) []string {
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
		beforeAdmin, hasBeforeAdmin := base.Administration[id]
		afterAgent, hasAfterAgent := after.Agents[id]
		afterAdmin, hasAfterAdmin := after.Administration[id]
		beforePeer := hasBeforeAgent && peerVisible(beforeAgent, beforeAdmin, hasBeforeAdmin)
		afterPeer := hasAfterAgent && peerVisible(afterAgent, afterAdmin, hasAfterAdmin)
		if beforePeer != afterPeer || afterPeer && hasBeforeAgent && peerFieldsChanged(beforeAgent, afterAgent) {
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

func peerVisible(agent journal.AgentRegistration, admin journal.AgentAdministration, hasAdmin bool) bool {
	if hasAdmin && (admin.LifecycleState == "retired" || admin.CredentialRevokedAt != nil) {
		return false
	}
	return agent.WireguardPublicKey != "" && agent.WireguardEndpoint != "" && agent.WorkloadIPv4Subnet != "" && agent.WorkloadIPv6Subnet != ""
}

func peerFieldsChanged(before, after journal.AgentRegistration) bool {
	return before.Name != after.Name ||
		before.WorkloadIPv4Subnet != after.WorkloadIPv4Subnet ||
		before.WorkloadIPv6Subnet != after.WorkloadIPv6Subnet ||
		before.WireguardPublicKey != after.WireguardPublicKey ||
		before.WireguardEndpoint != after.WireguardEndpoint
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
