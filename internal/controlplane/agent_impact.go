package controlplane

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// bumpAffectedAgents advances desired_revision for exactly the agents whose
// desired snapshot can change because of the durable rows this command
// recorded. The bump runs inside the same command transaction, so it commits
// with the product change and is itself recorded for replay.
//
// Assignments and peer-visible agent/admin fields (WireGuard key/port/endpoint,
// overlay subnets, name, retire/revoke) are cluster-wide: every snapshot embeds
// the identity catalog and a full-mesh peer list. Environments, projects,
// volumes, services, and revisions are scoped to agents that currently host
// that environment. Rollouts and domains appear only on the hosting agent's
// own DesiredService, so they stay service-scoped. Session, capacity, software,
// timestamps, and deployment staging do not appear in a snapshot.
func (s *database) bumpAffectedAgents(ctx context.Context, tx *sql.Tx, base journal.DurableState, batch journal.Batch) error {
	recorder := journal.RecorderFromContext(ctx)
	var domainServices map[string][]string
	if recorder != nil {
		domainServices = recorder.DomainServices()
	}
	agentIDs, err := affectedAgentIDs(ctx, tx, base, batch, domainServices)
	if err != nil {
		return err
	}
	if len(agentIDs) == 0 {
		return nil
	}
	return dbtx.BumpDesiredRevisions(ctx, tx, agentIDs)
}

func affectedAgentIDs(ctx context.Context, tx *sql.Tx, base journal.DurableState, batch journal.Batch, domainServices map[string][]string) ([]string, error) {
	if len(batch.Assignments) > 0 || agentDeltaRequiresClusterFanout(base, batch) {
		return allAgentIDs(ctx, tx)
	}

	environments := make([]string, 0, len(batch.Environments)+len(batch.Volumes)+len(batch.Services))
	for _, change := range batch.Environments {
		environments = append(environments, change.Key)
	}
	environments = append(environments, volumeEnvironmentIDs(base, batch)...)
	environments = append(environments, serviceEnvironmentIDs(base, batch, serviceIDsFromEnvScoped(batch))...)

	serviceScoped := map[string]struct{}{}
	for _, change := range batch.Rollouts {
		if serviceID, _, ok := strings.Cut(change.Key, "/"); ok {
			serviceScoped[serviceID] = struct{}{}
		}
	}
	for _, services := range domainServices {
		for _, serviceID := range services {
			serviceScoped[serviceID] = struct{}{}
		}
	}
	unresolvedDomains := changedDomainsWithoutServiceHint(batch, domainServices)
	if len(unresolvedDomains) > 0 {
		resolved, err := servicesForDomainHostnames(ctx, tx, unresolvedDomains)
		if err != nil {
			return nil, err
		}
		for _, serviceID := range resolved {
			serviceScoped[serviceID] = struct{}{}
		}
	}

	projects := make([]string, 0, len(batch.Projects))
	for _, change := range batch.Projects {
		projects = append(projects, change.Key)
	}

	var out []string
	out = append(out, selfAgentIDs(base, batch)...)
	if ids := uniqueStrings(environments); len(ids) > 0 {
		found, err := agentIDsForEnvironments(ctx, tx, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if ids := keysOf(serviceScoped); len(ids) > 0 {
		found, err := agentIDsForServices(ctx, tx, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if len(projects) > 0 {
		found, err := agentIDsForProjects(ctx, tx, projects)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	sort.Strings(out)
	return dedupe(out), nil
}

func serviceIDsFromEnvScoped(batch journal.Batch) []string {
	ids := map[string]struct{}{}
	for _, change := range batch.Services {
		ids[change.Key] = struct{}{}
	}
	for _, change := range batch.Revisions {
		if serviceID, _, ok := strings.Cut(change.Key, "/"); ok {
			ids[serviceID] = struct{}{}
		}
	}
	return keysOf(ids)
}

func volumeEnvironmentIDs(base journal.DurableState, batch journal.Batch) []string {
	var out []string
	for _, change := range batch.Volumes {
		if change.Value != nil {
			out = append(out, change.Value.EnvironmentID)
			continue
		}
		if volume, ok := base.Volumes[change.Key]; ok {
			out = append(out, volume.EnvironmentID)
		}
	}
	return out
}

func serviceEnvironmentIDs(base journal.DurableState, batch journal.Batch, serviceIDs []string) []string {
	fromBatch := map[string]string{}
	for _, change := range batch.Services {
		if change.Value != nil {
			fromBatch[change.Key] = change.Value.EnvironmentID
		}
	}
	var out []string
	for _, id := range serviceIDs {
		if envID, ok := fromBatch[id]; ok {
			out = append(out, envID)
			continue
		}
		if service, ok := base.Services[id]; ok {
			out = append(out, service.EnvironmentID)
		}
	}
	return out
}

func agentDeltaRequiresClusterFanout(base journal.DurableState, batch journal.Batch) bool {
	for _, id := range changedAgentIDs(batch) {
		beforeAgent, hasBeforeAgent := base.Agents[id]
		beforeAdmin, hasBeforeAdmin := base.Administration[id]
		afterAgent, hasAfterAgent := agentAfter(base, batch, id)
		afterAdmin, hasAfterAdmin := adminAfter(base, batch, id)
		beforePeer := hasBeforeAgent && peerVisible(beforeAgent, beforeAdmin, hasBeforeAdmin)
		afterPeer := hasAfterAgent && peerVisible(afterAgent, afterAdmin, hasAfterAdmin)
		if beforePeer != afterPeer {
			return true
		}
		if afterPeer && hasBeforeAgent && peerFieldsChanged(beforeAgent, afterAgent) {
			return true
		}
	}
	return false
}

func selfAgentIDs(base journal.DurableState, batch journal.Batch) []string {
	var out []string
	for _, id := range changedAgentIDs(batch) {
		afterAgent, hasAfter := agentAfter(base, batch, id)
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

func agentAfter(base journal.DurableState, batch journal.Batch, id string) (journal.AgentRegistration, bool) {
	for _, change := range batch.Agents {
		if change.Key != id {
			continue
		}
		if change.Value == nil {
			return journal.AgentRegistration{}, false
		}
		return *change.Value, true
	}
	agent, ok := base.Agents[id]
	return agent, ok
}

func adminAfter(base journal.DurableState, batch journal.Batch, id string) (journal.AgentAdministration, bool) {
	for _, change := range batch.Administration {
		if change.Key != id {
			continue
		}
		if change.Value == nil {
			return journal.AgentAdministration{}, false
		}
		return *change.Value, true
	}
	admin, ok := base.Administration[id]
	return admin, ok
}

func peerVisible(agent journal.AgentRegistration, admin journal.AgentAdministration, hasAdmin bool) bool {
	if hasAdmin && (admin.LifecycleState == "retired" || admin.CredentialRevokedAt != nil) {
		return false
	}
	return agent.WireguardPublicKey != "" && agent.WireguardListenPort > 0 && agent.WorkloadIPv4Subnet != "" && agent.WorkloadIPv6Subnet != ""
}

func peerFieldsChanged(before, after journal.AgentRegistration) bool {
	return before.Name != after.Name ||
		before.AdvertiseAddr != after.AdvertiseAddr ||
		before.WorkloadIPv4Subnet != after.WorkloadIPv4Subnet ||
		before.WorkloadIPv6Subnet != after.WorkloadIPv6Subnet ||
		before.WireguardPublicKey != after.WireguardPublicKey ||
		before.WireguardListenPort != after.WireguardListenPort
}

func selfNodeConfigChanged(before journal.AgentRegistration, hasBefore bool, after journal.AgentRegistration) bool {
	if !hasBefore {
		return after.WireguardIPv6 != ""
	}
	return before.WireguardIPv6 != after.WireguardIPv6
}

func changedDomainsWithoutServiceHint(batch journal.Batch, domainServices map[string][]string) []string {
	var out []string
	for _, change := range batch.Domains {
		if len(domainServices[change.Key]) == 0 {
			out = append(out, change.Key)
		}
	}
	return out
}

func allAgentIDs(ctx context.Context, tx *sql.Tx) ([]string, error) {
	return queryStrings(ctx, tx, `SELECT id::STRING FROM agent_registrations`)
}

func agentIDsForServices(ctx context.Context, tx *sql.Tx, serviceIDs []string) ([]string, error) {
	predicate, args := stringIn("service_id", serviceIDs)
	return queryStrings(ctx, tx, `SELECT DISTINCT agent_id FROM allocation_assignments WHERE `+predicate, args...)
}

func agentIDsForEnvironments(ctx context.Context, tx *sql.Tx, environmentIDs []string) ([]string, error) {
	predicate, args := stringIn("s.environment_id", environmentIDs)
	return queryStrings(ctx, tx, `
		SELECT DISTINCT a.agent_id
		  FROM allocation_assignments a
		  JOIN services s ON s.id = a.service_id
		 WHERE `+predicate, args...)
}

func agentIDsForProjects(ctx context.Context, tx *sql.Tx, projectIDs []string) ([]string, error) {
	predicate, args := stringIn("e.project_id", projectIDs)
	return queryStrings(ctx, tx, `
		SELECT DISTINCT a.agent_id
		  FROM allocation_assignments a
		  JOIN services s ON s.id = a.service_id
		  JOIN environments e ON e.id = s.environment_id
		 WHERE `+predicate, args...)
}

func servicesForDomainHostnames(ctx context.Context, tx *sql.Tx, hostnames []string) ([]string, error) {
	predicate, args := stringIn("hostname", hostnames)
	return queryStrings(ctx, tx, `SELECT DISTINCT service_id FROM domain_bindings WHERE `+predicate, args...)
}

func queryStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func stringIn(column string, values []string) (string, []any) {
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, value := range values {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = value
	}
	return column + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

func keysOf(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		seen[value] = struct{}{}
	}
	return keysOf(seen)
}

func dedupe(sorted []string) []string {
	if len(sorted) == 0 {
		return nil
	}
	out := sorted[:1]
	for _, value := range sorted[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
