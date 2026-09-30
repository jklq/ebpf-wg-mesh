package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/restartpolicy"
)

func CanonicalAgentWireGuardEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	endpoint, err := netip.ParseAddrPort(raw)
	if err != nil {
		return "", fmt.Errorf("wireguard_endpoint must be an IP:port endpoint: %q", raw)
	}
	if endpoint.Port() == 0 {
		return "", fmt.Errorf("wireguard_endpoint must include a non-zero port: %q", raw)
	}
	addr := endpoint.Addr().Unmap()
	if !isUsableUnderlayAddress(addr) {
		return "", fmt.Errorf("wireguard_endpoint must be a routable unicast address: %q", raw)
	}
	return netip.AddrPortFrom(addr, endpoint.Port()).String(), nil
}

func CanonicalAgentAdvertiseAddr(raw string) (string, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("advertise_addr must be an IPv6 address: %q", raw)
	}
	addr = addr.Unmap()
	if !addr.Is6() {
		return "", fmt.Errorf("advertise_addr must be an IPv6 address: %q", raw)
	}
	if !isUsableUnderlayAddress(addr) {
		return "", fmt.Errorf("advertise_addr must be a routable unicast address: %q", raw)
	}
	return addr.String(), nil
}

// isUsableUnderlayAddress rejects unspecified, loopback, link-local and
// multicast addresses so an agent cannot redirect peer handshakes at martian
// or local-only destinations.
func isUsableUnderlayAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return false
	}
	return true
}

func (d *Delivery) RegisterAgent(ctx context.Context, hello *agentv1.AgentHello) (bool, error) {
	if hello.GetWireguardListenPort() < 1 || hello.GetWireguardListenPort() > math.MaxUint16 {
		return false, fmt.Errorf("wireguard listen port must be between 1 and 65535")
	}
	// Persisted agent metadata remains active across a control-plane restart, but
	// the missing live session still makes the agent unavailable to placement.
	becameReachable := true
	if agent, ok := d.live.Agent(hello.GetAgentId()); ok {
		becameReachable = agent.LifecycleState == AgentStateUnavailable
	}
	s := d.store
	var changed bool
	var wireGuardEndpoint string
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}

		var lockedAgentID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_registrations WHERE id = $1 FOR UPDATE`, hello.GetAgentId()).Scan(&lockedAgentID); errors.Is(err, sql.ErrNoRows) {
			return ErrAgentNotEnrolled
		} else if err != nil {
			return err
		}
		existing, err := agentByIDQuerier(ctx, tx, lockedAgentID, false)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAgentNotEnrolled
		}
		if err != nil {
			return err
		}
		if existing.LifecycleState == AgentStateRetired || existing.CredentialRevokedAt.Valid {
			return ErrAgentCredentialRevoked
		}
		wireGuardEndpoint, err = CanonicalAgentWireGuardEndpoint(hello.GetWireguardEndpoint())
		if err != nil {
			return err
		}
		advertiseAddr, err := CanonicalAgentAdvertiseAddr(hello.GetAdvertiseAddr())
		if err != nil {
			return err
		}
		hello.AdvertiseAddr = advertiseAddr

		if hello.GetLocalStoreId() == "" {
			return fmt.Errorf("local_store_id is required")
		}
		var storeID string
		var previousIncarnation uint64
		if err := tx.QueryRowContext(ctx, `SELECT local_store_id, session_incarnation FROM agent_registrations WHERE id = $1`, hello.GetAgentId()).Scan(&storeID, &previousIncarnation); err != nil {
			return err
		}
		if storeID != "" && storeID != hello.GetLocalStoreId() {
			return fmt.Errorf("%w: local store differs from enrolled store", reconciliation.ErrIdentityRecovery)
		}
		if hello.GetSessionIncarnation() <= previousIncarnation || hello.GetSessionIncarnation() > math.MaxInt64 {
			return ErrStaleAgentSession
		}
		if _, err := journal.AgentRow(hello.GetAgentId()).Exec(ctx, tx, `UPDATE agent_registrations SET session_incarnation = $2 WHERE id = $1`, hello.GetAgentId(), int64(hello.GetSessionIncarnation())); err != nil {
			return err
		}
		if storeID == "" {
			if _, err := journal.AgentRow(hello.GetAgentId()).Exec(ctx, tx, `UPDATE agent_registrations SET local_store_id = $2 WHERE id = $1`, hello.GetAgentId(), hello.GetLocalStoreId()); err != nil {
				return err
			}
		}
		workloadIPv4Subnet := existing.WorkloadIPv4Subnet
		if workloadIPv4Subnet == "" {
			workloadIPv4Subnet, err = s.allocateWorkloadIPv4SubnetTx(ctx, tx)
			if err != nil {
				return err
			}
		}
		addressesBackfilled, err := d.backfillWorkloadIPv4AddressesTx(ctx, tx, existing.ID, workloadIPv4Subnet, now)
		if err != nil {
			return err
		}
		workloadSubnet := existing.WorkloadIPv6Subnet
		if workloadSubnet == "" {
			workloadSubnet, err = s.allocateWorkloadSubnetTx(ctx, tx, hello.AgentId)
			if err != nil {
				return err
			}
		}
		wireGuardIPv6 := existing.WireGuardIPv6
		if wireGuardIPv6 == "" {
			wireGuardIPv6, err = s.allocateWireGuardIPv6Tx(ctx, tx, hello.AgentId)
			if err != nil {
				return err
			}
		}

		capabilities := CanonicalCapabilities(hello.GetRuntimeCapabilities())
		changed = existing.AdvertiseAddr != hello.AdvertiseAddr ||
			existing.WireGuardPublicKey != hello.GetWireguardPublicKey() ||
			existing.WireGuardListenPort != int(hello.GetWireguardListenPort()) ||
			existing.WireGuardEndpoint != wireGuardEndpoint ||
			existing.CPUMillisCapacity != hello.CpuMillisCapacity ||
			existing.MemoryMebibytesCapcity != hello.MemoryMebibytesCapacity ||
			!slices.Equal(existing.RuntimeCapabilities, capabilities) ||
			existing.SoftwareVersion != strings.TrimSpace(hello.GetSoftwareVersion()) ||
			existing.LifecycleState == AgentStateEnrolling || existing.LifecycleState == AgentStateUnavailable ||
			addressesBackfilled ||
			existing.WorkloadIPv4Subnet != workloadIPv4Subnet ||
			existing.WorkloadIPv6Subnet != workloadSubnet ||
			existing.WireGuardIPv6 != wireGuardIPv6

		capabilitiesJSON, err := json.Marshal(capabilities)
		if err != nil {
			return err
		}
		_, err = journal.AgentRow(hello.GetAgentId()).Exec(ctx, tx,
			`UPDATE agent_registrations SET
				advertise_addr = $1, workload_ipv4_subnet = $2, workload_ipv6_subnet = $3, wireguard_public_key = $4,
				wireguard_listen_port = $5, wireguard_endpoint = $6, wireguard_ipv6 = $7, cpu_millis_capacity = $8,
				memory_mebibytes_capacity = $9, runtime_capabilities = $10,
				software_version = $11, updated_at = $12
			 WHERE id = $13`,
			hello.AdvertiseAddr,
			workloadIPv4Subnet,
			workloadSubnet,
			hello.GetWireguardPublicKey(),
			hello.GetWireguardListenPort(),
			wireGuardEndpoint,
			wireGuardIPv6,
			hello.CpuMillisCapacity,
			hello.MemoryMebibytesCapacity,
			capabilitiesJSON,
			strings.TrimSpace(hello.GetSoftwareVersion()),
			now, hello.AgentId,
		)
		if err != nil {
			return err
		}

		administration, err := journal.AdministrationRow(hello.GetAgentId()).Exec(ctx, tx, `UPDATE agent_administration
			SET lifecycle_state = CASE WHEN lifecycle_state = 'enrolling' THEN 'active' ELSE lifecycle_state END,
			    updated_at = CASE WHEN lifecycle_state = 'enrolling' THEN $2 ELSE updated_at END
			WHERE agent_id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, hello.GetAgentId(), now)
		if err != nil {
			return err
		}
		rows, err := administration.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrAgentCredentialRevoked
		}

		return nil
	})
	if err != nil {
		return false, err
	}
	assigned := assignedAllocationIDs(d, ctx, hello.GetAgentId())
	inventory := make([]string, 0, len(hello.GetAllocations()))
	for _, cond := range hello.GetAllocations() {
		inventory = append(inventory, cond.GetAllocationId())
	}
	if err := d.live.BeginSession(hello.GetAgentId(), hello.GetSessionId(), inventory, assigned, !hello.GetRecoveryMode()); err != nil {
		return false, err
	}
	d.live.InitSessionVersions(hello.GetAgentId(), hello.GetSessionId(), SyncVersions{
		Cursor:      hello.GetReconciliationCursor(),
		NodeConfig:  hello.GetAcceptedNodeConfigVersion(),
		Credentials: hello.GetAcceptedCredentialsVersion(),
		Replicas:    hello.GetAcceptedReplicasVersion(),
	})
	return changed || becameReachable, nil
}

func assignedAllocationIDs(d *Delivery, ctx context.Context, agentID string) []string {
	_ = ctx
	if d == nil || d.live == nil {
		return nil
	}
	return d.live.AssignedIDs(agentID)
}

func (d *Delivery) ObserveAgentHeartbeat(ctx context.Context, agentID, sessionID string, recoveryMode bool) error {
	_ = ctx
	return d.live.Heartbeat(agentID, sessionID, !recoveryMode)
}

func (d *Delivery) EndAgentSession(ctx context.Context, agentID, sessionID string) error {
	_ = ctx
	return d.live.EndSession(agentID, sessionID)
}

func (d *Delivery) ObserveAgentStatus(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) error {
	ingressChanged, _, err := d.recordStatusReport(ctx, authenticatedAgentID, report)
	if err != nil {
		return err
	}
	if ingressChanged && d.ingress != nil && d.live.Publishing() {
		d.ingress.RequestSync()
	}
	return nil
}

func (d *Delivery) recordStatusReport(ctx context.Context, authenticatedAgentID string, report *agentv1.StatusReport) (bool, []string, error) {
	if !d.live.Serving() {
		return false, nil, ErrNotLiveOwner
	}
	if report == nil || strings.TrimSpace(report.GetAgentId()) == "" || report.GetAgentId() != authenticatedAgentID {
		return false, nil, fmt.Errorf("%w: report agent_id does not match authenticated agent", ErrAllocationOwnership)
	}
	inventory := make([]string, 0, len(report.GetServices()))
	for _, cond := range report.GetServices() {
		inventory = append(inventory, cond.GetAllocationId())
	}

	product := d.live.Product()
	durable := product.DurableState
	if err := validateStatusInventory(durable, authenticatedAgentID, report); err != nil {
		return false, nil, err
	}
	if err := d.live.AcceptReport(authenticatedAgentID, report.GetSessionId(), report.GetObservationSequence(), inventory, !report.GetRecoveryMode()); err != nil {
		return false, nil, err
	}

	now := d.live.currentTime()
	ingressChanged := false
	statusInvalidated := false
	stateChanged := false
	changedEnvironments := make(map[string]struct{})
	rolloutServiceIDs := make(map[string]struct{})
	deploymentAllocationIDs := make(map[string]struct{})
	for _, cond := range report.GetServices() {
		assignment := durable.Assignments[cond.GetAllocationId()]
		generation := cond.GetDesiredRolloutGeneration()

		phase, healthy := cond.GetPhase(), cond.GetHealthy()
		if cond.GetRestart().GetCrashLoop() || phase == restartpolicy.PhaseCrashLoop {
			phase, healthy = restartpolicy.PhaseCrashLoop, false
		}
		observation := AllocationObservation{
			AllocationID: cond.GetAllocationId(), RolloutGeneration: generation,
			AppliedSpecRevision: cond.GetAppliedSpecRevision(), AppliedGeneration: cond.GetAppliedRolloutGeneration(),
			Phase: phase, Message: cond.GetMessage(), Healthy: healthy,
			HealthyIPv4Ports: cond.GetHealthyIpv4Ports(), HealthyIPv6Ports: cond.GetHealthyIpv6Ports(), Restart: cond.GetRestart(),
			AgentID: authenticatedAgentID, SessionID: report.GetSessionId(), Sequence: report.GetObservationSequence(), ObservedAt: now,
		}
		outcome, err := d.live.RecordObservation(observation)
		if err != nil {
			return false, nil, err
		}
		// Only the desired generation renders into status; stale ones never invalidate.
		if outcome.StatusInvalidated && generation == assignment.DesiredRolloutGeneration {
			statusInvalidated = true
		}
		if !outcome.Changed || generation != assignment.DesiredRolloutGeneration {
			continue
		}
		service := durable.Services[assignment.ServiceID]
		changedEnvironments[service.EnvironmentID] = struct{}{}
		rollout := durable.Rollouts[fmt.Sprintf("%s/%d", assignment.ServiceID, generation)]
		if rollout.State == rolloutStateInProgress {
			rolloutServiceIDs[assignment.ServiceID] = struct{}{}
		} else {
			deploymentAllocationIDs[cond.GetAllocationId()] = struct{}{}
		}
		if len(product.DomainHostnamesForService(assignment.ServiceID)) > 0 {
			ingressChanged = true
		}
	}
	for allocationID := range deploymentAllocationIDs {
		changed, err := d.evaluateObservedDeployment(ctx, allocationID)
		if err != nil {
			return false, nil, fmt.Errorf("evaluate deployment after observation: %w", err)
		}
		stateChanged = stateChanged || changed
	}
	for serviceID := range rolloutServiceIDs {
		advanced, advanceErr := d.advanceRollout(ctx, serviceID, now)
		if advanceErr != nil {
			return false, nil, fmt.Errorf("advance rollout after status: %w", advanceErr)
		}
		stateChanged = stateChanged || advanced.Changed
		ingressChanged = ingressChanged || advanced.IngressChanged
		if advanced.EnvironmentID != "" {
			changedEnvironments[advanced.EnvironmentID] = struct{}{}
		}
	}
	if statusInvalidated && !stateChanged {
		// Rendered status changed but no transaction touched durable state, so no revision was
		// bumped; publish an explicit invalidation or subscribers keep showing stale status.
		if err := d.publishStatusInvalidation(ctx); err != nil {
			return false, nil, fmt.Errorf("publish status invalidation: %w", err)
		}
	}
	environmentIDs := make([]string, 0, len(changedEnvironments))
	for environmentID := range changedEnvironments {
		environmentIDs = append(environmentIDs, environmentID)
	}
	return ingressChanged, environmentIDs, nil
}

// publishStatusInvalidation wakes status subscribers after observation changes with no
// durable state change. The revision bump is the invalidation signal.
func (d *Delivery) publishStatusInvalidation(ctx context.Context) error {
	if d.store == nil || d.store.withObservationTx == nil {
		return nil
	}
	return d.store.withObservationTx(ctx, func(context.Context, *sql.Tx) (bool, error) {
		return true, nil
	})
}

func validateStatusInventory(durable journal.DurableState, authenticatedAgentID string, report *agentv1.StatusReport) error {
	for _, cond := range report.GetServices() {
		if cond.GetAllocationId() == "" || cond.GetServiceId() == "" {
			return fmt.Errorf("%w: allocation_id and service_id are required", ErrAllocationOwnership)
		}
		assignment, ok := durable.Assignments[cond.GetAllocationId()]
		if !ok || assignment.AgentID != authenticatedAgentID {
			return fmt.Errorf("%w: allocation %q", ErrAllocationOwnership, cond.GetAllocationId())
		}
		if cond.GetServiceId() != assignment.ServiceID {
			return fmt.Errorf("%w: allocation %q belongs to service %q", ErrAllocationOwnership, cond.GetAllocationId(), assignment.ServiceID)
		}
		generation := cond.GetDesiredRolloutGeneration()
		if generation <= 0 || generation > assignment.DesiredRolloutGeneration {
			return fmt.Errorf("%w: allocation %q generation %d is not assigned (current %d)", ErrAllocationOwnership, cond.GetAllocationId(), generation, assignment.DesiredRolloutGeneration)
		}
		if cond.GetAppliedRolloutGeneration() > generation {
			return fmt.Errorf("%w: applied generation %d exceeds observed generation %d", ErrAllocationOwnership, cond.GetAppliedRolloutGeneration(), generation)
		}
		if generation == assignment.DesiredRolloutGeneration && (cond.GetDesiredSpecRevision() != assignment.DesiredSpecRevision || cond.GetAppliedSpecRevision() > assignment.DesiredSpecRevision) {
			return fmt.Errorf("%w: allocation %q spec revision %d/%d does not match assignment %d", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetDesiredSpecRevision(), cond.GetAppliedSpecRevision(), assignment.DesiredSpecRevision)
		}
		if strings.TrimSpace(cond.GetAllocationIpv4()) != assignment.AllocationIPv4 || strings.TrimSpace(cond.GetAllocationIpv6()) != assignment.AllocationIPv6 {
			return fmt.Errorf("%w: allocation %s reported addresses %q/%q, assigned %q/%q", ErrAllocationOwnership, cond.GetAllocationId(), cond.GetAllocationIpv4(), cond.GetAllocationIpv6(), assignment.AllocationIPv4, assignment.AllocationIPv6)
		}
	}
	return nil
}

// evaluateObservedDeployment folds a live observation into the deployment record and reports
// whether durable state changed. Terminal deployments yield no transition, so the caller must
// invalidate status subscribers explicitly.
func (d *Delivery) evaluateObservedDeployment(ctx context.Context, allocationID string) (bool, error) {
	d.schedulerMu.Lock()
	defer d.schedulerMu.Unlock()
	var changed bool
	err := d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var serviceID, agentID string
		var desired int64
		err := tx.QueryRowContext(ctx, `SELECT service_id, agent_id, desired_rollout_generation
			FROM allocation_assignments WHERE id = $1`, allocationID).Scan(&serviceID, &agentID, &desired)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		obs, ok := d.live.Observation(allocationID, desired)
		if !ok {
			return nil
		}
		session, ok := d.live.Session(agentID)
		if !ok || session.SessionID != obs.SessionID || obs.AgentID != agentID {
			return nil
		}
		transitioned, err := d.store.applyAgentDeploymentObservationTx(ctx, tx, serviceID, desired, obs.Phase, obs.Message, obs.Healthy, obs.AppliedGeneration, agentID)
		if err != nil {
			return err
		}
		changed = transitioned
		return nil
	})
	return changed, err
}

func (s *persistence) listAgents(ctx context.Context) ([]AgentRecord, error) {
	_ = ctx
	if s != nil && s.live != nil {
		return s.live.AgentsIfServing()
	}
	return s.listAgentsQuerier(ctx, s.db)
}

func (s *persistence) agentByID(ctx context.Context, agentID string) (AgentRecord, error) {
	if s != nil && s.live != nil {
		return s.live.AgentIfServing(agentID)
	}
	rec, err := agentByIDQuerier(ctx, s.db, agentID, false)
	if err != nil {
		return AgentRecord{}, err
	}
	return s.overlayAgent(rec), nil
}

func (s *persistence) agentIDs(ctx context.Context) ([]string, error) {
	if s != nil && s.live != nil {
		return s.live.AgentIDsIfServing()
	}
	return s.agentIDsQuerier(ctx, s.db)
}

func (s *persistence) agentIDsQuerier(ctx context.Context, q ServiceQueryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM agents ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *persistence) allocateWorkloadSubnetTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT workload_ipv6_subnet FROM agents WHERE workload_ipv6_subnet <> ''`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		used[raw] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return nextSubnetFromPool(s.mesh.WorkloadPoolCIDR, 64, used, agentID)
}

func (s *persistence) allocateWorkloadIPv4SubnetTx(ctx context.Context, tx *sql.Tx) (string, error) {
	pool := s.mesh.WorkloadIPv4PoolCIDR
	prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
	var configuredPool string
	var configuredBits int
	var nextOrdinal int64
	err := tx.QueryRowContext(ctx, `SELECT pool_cidr, prefix_bits, next_ordinal
		FROM workload_ipv4_prefix_allocator WHERE id = TRUE FOR UPDATE`).Scan(&configuredPool, &configuredBits, &nextOrdinal)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workload_ipv4_prefix_allocator(id, pool_cidr, prefix_bits, next_ordinal)
			VALUES (TRUE, $1, $2, 0)`, pool, prefixBits); err != nil {
			return "", err
		}
		configuredPool, configuredBits, nextOrdinal = pool, prefixBits, 0
	} else if err != nil {
		return "", err
	}
	if configuredPool != pool || configuredBits != prefixBits {
		return "", fmt.Errorf("IPv4 workload pool configuration changed from %s /%d to %s /%d", configuredPool, configuredBits, pool, prefixBits)
	}

	poolPrefix, err := netip.ParsePrefix(pool)
	if err != nil || !poolPrefix.Addr().Is4() {
		return "", fmt.Errorf("invalid IPv4 workload pool %q", pool)
	}
	poolPrefix = poolPrefix.Masked()
	rows, err := tx.QueryContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE workload_ipv4_subnet <> '' ORDER BY workload_ipv4_subnet`)
	if err != nil {
		return "", err
	}
	var used []netip.Prefix
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return "", err
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() != prefixBits || !poolPrefix.Contains(prefix.Masked().Addr()) {
			rows.Close()
			return "", fmt.Errorf("allocated IPv4 node prefix %q is outside configured pool %q", raw, pool)
		}
		prefix = prefix.Masked()
		for _, other := range used {
			if IPv4PrefixesOverlap(prefix, other) {
				rows.Close()
				return "", fmt.Errorf("allocated IPv4 node prefixes %s and %s overlap", prefix, other)
			}
		}
		used = append(used, prefix)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	candidateRaw, err := IPv4SubnetAt(pool, prefixBits, uint64(nextOrdinal))
	if err != nil {
		return "", err
	}
	candidate, _ := netip.ParsePrefix(candidateRaw)
	for _, other := range used {
		if IPv4PrefixesOverlap(candidate, other) {
			return "", fmt.Errorf("next IPv4 node prefix %s overlaps allocated prefix %s", candidate, other)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workload_ipv4_prefix_allocator SET next_ordinal = $1 WHERE id = TRUE`, nextOrdinal+1); err != nil {
		return "", err
	}
	return candidateRaw, nil
}

func (s *persistence) allocateWorkloadIPv4AddressTx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	var subnet string
	if err := tx.QueryRowContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&subnet); err != nil {
		return "", err
	}
	if strings.TrimSpace(subnet) == "" {
		return "", fmt.Errorf("agent %s has no IPv4 workload prefix", agentID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT allocation_ipv4 FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> $2 AND allocation_ipv4 <> ''`, agentID, AllocationRolloutLost)
	if err != nil {
		return "", err
	}
	used := make(map[string]struct{})
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			rows.Close()
			return "", err
		}
		used[addr] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	return nextIPv4AddressFromSubnet(subnet, used)
}

func (d *Delivery) backfillWorkloadIPv4AddressesTx(ctx context.Context, tx *sql.Tx, agentID, subnet string, now time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, allocation_ipv4 FROM allocation_assignments
		WHERE agent_id = $1 AND rollout_state <> $2 ORDER BY created_at ASC, id ASC FOR UPDATE`, agentID, AllocationRolloutLost)
	if err != nil {
		return false, err
	}
	used := make(map[string]struct{})
	var missing []string
	for rows.Next() {
		var allocationID, address string
		if err := rows.Scan(&allocationID, &address); err != nil {
			rows.Close()
			return false, err
		}
		address = strings.TrimSpace(address)
		if address == "" {
			missing = append(missing, allocationID)
			continue
		}
		used[address] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	mutations := make([]allocationMutation, 0, len(missing))
	for _, allocationID := range missing {
		address, err := nextIPv4AddressFromSubnet(subnet, used)
		if err != nil {
			return false, err
		}
		mutations = append(mutations, allocationMutation{Kind: mutationReserveAddress, AllocationID: allocationID, Allocation: AllocationAssignment{IPv4: address}})
		used[address] = struct{}{}
	}
	if err := d.applyAllocationMutationsTx(ctx, tx, now, mutations...); err != nil {
		return false, err
	}
	return len(missing) > 0, nil
}

func (s *persistence) allocateWireGuardIPv6Tx(ctx context.Context, tx *sql.Tx, agentID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT wireguard_ipv6 FROM agents WHERE wireguard_ipv6 <> ''`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		used[raw] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return nextAddressFromPool(s.mesh.NetworkCIDR, used, 0x10, agentID)
}

func (s *persistence) listAgentsQuerier(ctx context.Context, q ServiceQueryer) ([]AgentRecord, error) {
	rows, err := q.QueryContext(ctx,
		agentSelectSQL+`
		  ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AgentRecord
	for rows.Next() {
		rec, err := scanAgentRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.overlayAgent(rec))
	}
	return out, rows.Err()
}
