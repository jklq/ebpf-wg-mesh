package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/reconciliation"

	"google.golang.org/protobuf/types/known/timestamppb"
)

type fleetLiveReader interface {
	AgentUsage() map[string]deliverycore.AgentUsage
	Position() deliverycore.LivePosition
	OrphanedVolumes(agentID string) []string
}

func (s *fleetPersistence) AuthorizeAgentCredential(ctx context.Context, agentID string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agents
		WHERE id = $1 AND lifecycle_state <> 'retired' AND credential_revoked_at IS NULL`, strings.TrimSpace(agentID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return deliverycore.ErrAgentCredentialRevoked
	}
	return err
}

func lifecycleStateProto(state deliverycore.AgentLifecycleState) platformv1.AgentLifecycleState {
	switch state {
	case deliverycore.AgentStateEnrolling:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ENROLLING
	case deliverycore.AgentStateActive:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE
	case deliverycore.AgentStateCordoned:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED
	case deliverycore.AgentStateDraining:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING
	case deliverycore.AgentStateUnavailable:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNAVAILABLE
	case deliverycore.AgentStateRetired:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED
	default:
		return platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_UNSPECIFIED
	}
}

func lifecycleStateRecord(state platformv1.AgentLifecycleState) deliverycore.AgentLifecycleState {
	switch state {
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_ACTIVE:
		return deliverycore.AgentStateActive
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_CORDONED:
		return deliverycore.AgentStateCordoned
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_DRAINING:
		return deliverycore.AgentStateDraining
	case platformv1.AgentLifecycleState_AGENT_LIFECYCLE_STATE_RETIRED:
		return deliverycore.AgentStateRetired
	default:
		return ""
	}
}

func (s *fleetPersistence) RecordAgentCertificate(ctx context.Context, agentID, serial string) error {
	agentID = strings.TrimSpace(agentID)
	serial = strings.ToLower(strings.TrimSpace(serial))
	if agentID == "" || serial == "" {
		return errors.New("agent certificate serial is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_certificates(serial, agent_id, issued_at)
		VALUES ($1, $2, $3) ON CONFLICT(serial) DO NOTHING`, serial, agentID, time.Now().UTC())
	return err
}

func (s *fleetPersistence) listAgentCertificateSerials(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial FROM agent_certificates WHERE agent_id = $1 ORDER BY issued_at`, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var serials []string
	for rows.Next() {
		var serial string
		if err := rows.Scan(&serial); err != nil {
			return nil, err
		}
		serials = append(serials, serial)
	}
	return serials, rows.Err()
}

func (s *fleetPersistence) fleetView(ctx context.Context, user authz.User) (*platformv1.Fleet, error) {
	// ListAgents already requires operator membership; no second check here.
	agents, err := s.reads.ListAgents(ctx, user)
	if err != nil {
		return nil, err
	}
	usageByAgent := s.live.AgentUsage()
	versions := make(map[string]int)
	for _, agent := range agents {
		if agent.LifecycleState != deliverycore.AgentStateRetired && agent.SoftwareVersion != "" {
			versions[agent.SoftwareVersion]++
		}
	}
	recommendedVersion := ""
	for version, count := range versions {
		if count > versions[recommendedVersion] || (count == versions[recommendedVersion] && version > recommendedVersion) {
			recommendedVersion = version
		}
	}
	fleet := &platformv1.Fleet{Capacity: &platformv1.FleetCapacity{}}
	now := time.Now().UTC()
	for _, rec := range agents {
		item := toProtoAgent(rec)
		used := usageByAgent[rec.ID]
		item.AllocationCount = used.Allocations
		item.AllocatedCpuMillis = used.CPUMillis
		item.AllocatedMemoryMebibytes = used.MemoryMebibytes
		item.HeadroomCpuMillis = max(item.SchedulableCpuMillis-used.CPUMillis, 0)
		item.HeadroomMemoryMebibytes = max(item.SchedulableMemoryMebibytes-used.MemoryMebibytes, 0)
		item.OrphanedVolumeIds = s.live.OrphanedVolumes(rec.ID)
		if recommendedVersion != "" && item.SoftwareVersion != "" && item.SoftwareVersion != recommendedVersion && rec.LifecycleState != deliverycore.AgentStateRetired {
			item.VersionSkewWarning = fmt.Sprintf("reports %s while the fleet majority reports %s", item.SoftwareVersion, recommendedVersion)
		}
		fleet.Agents = append(fleet.Agents, item)
		if rec.LifecycleState != deliverycore.AgentStateRetired {
			fleet.Capacity.NodeCount++
		}
		if rec.HostType != config.HostIntermittent && rec.LifecycleState == deliverycore.AgentStateActive && rec.Healthy(now) {
			fleet.Capacity.SchedulableNodeCount++
			fleet.Capacity.SchedulableCpuMillis += item.SchedulableCpuMillis
			fleet.Capacity.SchedulableMemoryMebibytes += item.SchedulableMemoryMebibytes
			fleet.Capacity.AllocatedCpuMillis += used.CPUMillis
			fleet.Capacity.AllocatedMemoryMebibytes += used.MemoryMebibytes
			fleet.Capacity.HeadroomCpuMillis += item.HeadroomCpuMillis
			fleet.Capacity.HeadroomMemoryMebibytes += item.HeadroomMemoryMebibytes
		}
	}
	if len(versions) > 1 {
		fleet.VersionWarning = fmt.Sprintf("fleet software version skew detected; converge nodes on %s", recommendedVersion)
	}
	if s.live != nil {
		fleet.Live = toProtoLiveRead(s.live.Position())
	}
	return fleet, nil
}

// scopeAgentLogBatch resolves the authoritative owner of every allocation in one agent log
// batch and scopes the batch onto it. Claims must match the owner when present; empty claims
// are filled from the owner and agent-supplied event metadata is cleared (platform events are
// server-generated), so a compromised agent can't attribute output or loss to another tenant.
// Lines for unowned or mismatched allocations are excluded individually with a warning: one bad
// line must never wedge delivery behind it, and an unverifiable line carries no gap row either.
func (s *fleetPersistence) scopeAgentLogBatch(ctx context.Context, agentID string, batch *agentv1.LogBatch) error {
	for _, entry := range batch.GetEntries() {
		entry.Event = ""
		entry.Attributes = nil
	}
	wanted := make(map[string]struct{}, len(batch.GetEntries())+len(batch.GetDrops()))
	for _, entry := range batch.GetEntries() {
		if id := entry.GetAllocationId(); id != "" {
			wanted[id] = struct{}{}
		}
	}
	for _, drop := range batch.GetDrops() {
		if id := drop.GetAllocationId(); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		batch.Entries = nil
		batch.Drops = nil
		return nil
	}
	ids := make([]string, 0, len(wanted))
	placeholders := make([]string, 0, len(wanted))
	args := make([]any, 0, len(wanted)+1)
	args = append(args, agentID)
	for id := range wanted {
		ids = append(ids, id)
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	type logOwner struct {
		environmentID string
		serviceID     string
	}
	owners := make(map[string]logOwner, len(ids))
	rows, err := s.db.QueryContext(ctx,
		`SELECT allocations.id, allocations.service_id, s.environment_id
		   FROM allocations
		   JOIN services s ON s.id = allocations.service_id
		  WHERE allocations.agent_id = $1
		    AND allocations.id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, serviceID, environmentID string
		if err := rows.Scan(&id, &serviceID, &environmentID); err != nil {
			return err
		}
		owners[id] = logOwner{environmentID: environmentID, serviceID: serviceID}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keptEntries := batch.Entries[:0]
	excluded := 0
	for _, entry := range batch.Entries {
		owner, ok := owners[entry.GetAllocationId()]
		if !ok {
			excluded++
			continue
		}
		if claimed := strings.TrimSpace(entry.GetServiceId()); claimed != "" && claimed != owner.serviceID {
			excluded++
			slog.Warn("dropped agent log line with mismatched service claim", "agent_id", agentID, "allocation_id", entry.GetAllocationId(), "claimed_service_id", claimed)
			continue
		}
		if claimed := strings.TrimSpace(entry.GetEnvironmentId()); claimed != "" && claimed != owner.environmentID {
			excluded++
			slog.Warn("dropped agent log line with mismatched environment claim", "agent_id", agentID, "allocation_id", entry.GetAllocationId(), "claimed_environment_id", claimed)
			continue
		}
		entry.ServiceId = owner.serviceID
		entry.EnvironmentId = owner.environmentID
		keptEntries = append(keptEntries, entry)
	}
	batch.Entries = keptEntries
	keptDrops := batch.Drops[:0]
	for _, drop := range batch.Drops {
		owner, ok := owners[drop.GetAllocationId()]
		if !ok {
			excluded++
			continue
		}
		if claimed := strings.TrimSpace(drop.GetServiceId()); claimed != "" && claimed != owner.serviceID {
			excluded++
			slog.Warn("dropped agent drop summary with mismatched service claim", "agent_id", agentID, "allocation_id", drop.GetAllocationId(), "claimed_service_id", claimed)
			continue
		}
		drop.ServiceId = owner.serviceID
		keptDrops = append(keptDrops, drop)
	}
	batch.Drops = keptDrops
	if excluded > 0 {
		slog.Warn("excluded unattributable agent log lines", "agent_id", agentID, "count", excluded)
	}
	return nil
}

func (s *fleetPersistence) validateWorkloadIPv4Pool(ctx context.Context) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		pool := s.mesh.WorkloadIPv4PoolCIDR
		prefixBits := s.mesh.WorkloadIPv4NodePrefixBits
		if strings.TrimSpace(pool) == "" || prefixBits == 0 {
			return errors.New("IPv4 workload pool and per-node prefix size are required")
		}
		if _, err := deliverycore.IPv4SubnetAt(pool, prefixBits, 0); err != nil {
			return err
		}
		var configuredPool string
		var configuredBits int
		var nextOrdinal int64
		err := tx.QueryRowContext(ctx, `SELECT pool_cidr, prefix_bits, next_ordinal
			FROM workload_ipv4_prefix_allocator WHERE id = TRUE FOR UPDATE`).Scan(&configuredPool, &configuredBits, &nextOrdinal)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO workload_ipv4_prefix_allocator(id, pool_cidr, prefix_bits, next_ordinal)
				VALUES (TRUE, $1, $2, 0)`, pool, prefixBits)
			return err
		}
		if err != nil {
			return err
		}
		if configuredPool != pool || configuredBits != prefixBits {
			return fmt.Errorf("IPv4 workload pool configuration changed from %s /%d to %s /%d", configuredPool, configuredBits, pool, prefixBits)
		}
		poolPrefix, err := netip.ParsePrefix(pool)
		if err != nil || !poolPrefix.Addr().Is4() {
			return fmt.Errorf("invalid IPv4 workload pool %q", pool)
		}
		poolPrefix = poolPrefix.Masked()
		rows, err := tx.QueryContext(ctx, `SELECT workload_ipv4_subnet FROM agents WHERE workload_ipv4_subnet <> '' ORDER BY workload_ipv4_subnet FOR UPDATE`)
		if err != nil {
			return err
		}
		var allocated []netip.Prefix
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() != prefixBits || !poolPrefix.Contains(prefix.Addr()) {
				rows.Close()
				return fmt.Errorf("allocated IPv4 node prefix %q is outside configured pool %q", raw, pool)
			}
			for _, other := range allocated {
				if deliverycore.IPv4PrefixesOverlap(prefix, other) {
					rows.Close()
					return fmt.Errorf("allocated IPv4 node prefixes %s and %s overlap", prefix, other)
				}
			}
			allocated = append(allocated, prefix)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if nextOrdinal < 0 {
			return fmt.Errorf("IPv4 workload allocator has invalid next ordinal %d", nextOrdinal)
		}
		return nil
	})
}

var errInvalidBootstrapToken = errors.New("invalid, consumed, or incorrectly bound bootstrap token")

func (s *fleetPersistence) ensureAgentBootstrapTokens(ctx context.Context, tokens []config.AgentBootstrapToken) error {
	return s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UTC()
		configured := make(map[string]struct{}, len(tokens))
		for _, bootstrap := range tokens {
			agentID := strings.TrimSpace(bootstrap.AgentID)
			token := strings.TrimSpace(bootstrap.Token)
			name := strings.TrimSpace(bootstrap.Name)
			if name == "" {
				name = agentID
			}
			region := strings.TrimSpace(bootstrap.Region)
			if region == "" {
				region = "default"
			}
			failureDomain := strings.TrimSpace(bootstrap.FailureDomain)
			if failureDomain == "" {
				failureDomain = strings.ToLower(agentID)
				failureDomain = strings.Map(func(r rune) rune {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
						return r
					}
					return '-'
				}, failureDomain)
			}
			if err := deliverycore.ValidateFleetAgentInput(agentID, name, region, bootstrap.Zone, failureDomain, bootstrap.ReservedCPUMillis, bootstrap.ReservedMemoryMebibytes); err != nil {
				return fmt.Errorf("configured agent %s: %w", agentID, err)
			}
			if _, err := journal.AgentRow(agentID).Exec(ctx, tx, `INSERT INTO agent_registrations(
				id, name, region, zone, failure_domain,
				reserved_cpu_millis, reserved_memory_mebibytes, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
			ON CONFLICT(id) DO NOTHING`, agentID, name, region, strings.TrimSpace(bootstrap.Zone), failureDomain,
				bootstrap.ReservedCPUMillis, bootstrap.ReservedMemoryMebibytes, now); err != nil {
				return fmt.Errorf("store configured fleet agent %s: %w", agentID, err)
			}
			if _, err := journal.AdministrationRow(agentID).Exec(ctx, tx, `INSERT INTO agent_administration(agent_id, lifecycle_state, updated_at)
				VALUES ($1, 'enrolling', $2) ON CONFLICT(agent_id) DO NOTHING`, agentID, now); err != nil {
				return err
			}
			hash := deliverycore.BootstrapTokenHash(token)
			configured[string(hash[:])] = struct{}{}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO agent_bootstrap_tokens(token_hash, agent_id, origin, created_at, consumed_at)
				 VALUES ($1, $2, 'config', $3, NULL)
				 ON CONFLICT(token_hash) DO NOTHING`,
				hash[:], agentID, now,
			); err != nil {
				return fmt.Errorf("store bootstrap token for agent %s: %w", agentID, err)
			}
			var persistedAgentID string
			if err := tx.QueryRowContext(ctx,
				`SELECT agent_id FROM agent_bootstrap_tokens WHERE token_hash = $1`,
				hash[:],
			).Scan(&persistedAgentID); err != nil {
				return fmt.Errorf("load bootstrap token binding: %w", err)
			}
			if persistedAgentID != agentID {
				return fmt.Errorf("bootstrap token is already bound to agent %s", persistedAgentID)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT token_hash FROM agent_bootstrap_tokens WHERE consumed_at IS NULL AND origin = 'config'`,
		)
		if err != nil {
			return fmt.Errorf("list active bootstrap tokens: %w", err)
		}
		var removed [][]byte
		for rows.Next() {
			var hash []byte
			if err := rows.Scan(&hash); err != nil {
				rows.Close()
				return fmt.Errorf("scan active bootstrap token: %w", err)
			}
			if _, ok := configured[string(hash)]; !ok {
				removed = append(removed, append([]byte(nil), hash...))
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close active bootstrap tokens: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate active bootstrap tokens: %w", err)
		}
		for _, hash := range removed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE agent_bootstrap_tokens SET consumed_at = $1 WHERE token_hash = $2 AND consumed_at IS NULL`,
				now, hash,
			); err != nil {
				return fmt.Errorf("revoke removed bootstrap token: %w", err)
			}
		}
		return nil
	})
}

func (s *fleetPersistence) ConsumeAgentBootstrapToken(ctx context.Context, agentID, token string) error {
	agentID = strings.TrimSpace(agentID)
	token = strings.TrimSpace(token)
	if agentID == "" || token == "" {
		return errInvalidBootstrapToken
	}
	hash := deliverycore.BootstrapTokenHash(token)
	result, err := s.db.ExecContext(ctx,
		`UPDATE agent_bootstrap_tokens
		    SET consumed_at = $1
		  WHERE token_hash = $2
		    AND agent_id = $3
		    AND consumed_at IS NULL`,
		time.Now().UTC(), hash[:], agentID,
	)
	if err != nil {
		return fmt.Errorf("consume agent bootstrap token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap token rows affected: %w", err)
	}
	if rows != 1 {
		return errInvalidBootstrapToken
	}
	return nil
}

func (s *fleetPersistence) ConsumeOrRecoverAgentBootstrapToken(ctx context.Context, agentID, token string, publicKeySHA256 []byte) error {
	agentID = strings.TrimSpace(agentID)
	token = strings.TrimSpace(token)
	if agentID == "" || token == "" || len(publicKeySHA256) != sha256.Size {
		return errInvalidBootstrapToken
	}
	hash := deliverycore.BootstrapTokenHash(token)
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`UPDATE agent_bootstrap_tokens
		    SET consumed_at = $1, csr_public_key_sha256 = $2
		  WHERE token_hash = $3
		    AND agent_id = $4
		    AND consumed_at IS NULL`,
		now, publicKeySHA256, hash[:], agentID,
	)
	if err != nil {
		return fmt.Errorf("consume agent bootstrap token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap token rows affected: %w", err)
	}
	if rows == 1 {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM agent_bootstrap_tokens
		  WHERE token_hash = $1
		    AND agent_id = $2
		    AND consumed_at IS NOT NULL
		    AND csr_public_key_sha256 = $3`,
		hash[:], agentID, publicKeySHA256,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errInvalidBootstrapToken
	}
	if err != nil {
		return fmt.Errorf("recover agent bootstrap token: %w", err)
	}
	return nil
}

type agentSessions interface {
	Serving() bool
	Grant(string, string, uint64, deliverycore.SyncVersions) error
	Acknowledge(string, string, uint64, deliverycore.SyncVersions) error
}

func (s *fleetPersistence) grantAgentCommand(ctx context.Context, agentID, sessionID string, epoch uint64, offered deliverycore.SyncVersions) (time.Time, error) {
	if s.sessions == nil {
		return time.Time{}, deliverycore.ErrNotLiveOwner
	}
	if err := s.sessions.Grant(agentID, sessionID, epoch, offered); err != nil {
		if errors.Is(err, deliverycore.ErrStaleAgentSession) {
			return time.Time{}, errors.New("agent session superseded")
		}
		return time.Time{}, err
	}
	var deadline time.Time
	err := s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&current); err != nil {
			return err
		}
		if uint64(current) != epoch {
			return errors.New("authority epoch changed; reconnect required")
		}
		if offered.Cursor < 0 {
			return errors.New("invalid desired-state cursor")
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		deadline = now.Add(reconciliation.GrantLifetime)
		_, err = tx.ExecContext(ctx, `UPDATE agent_authority SET outstanding_not_after = greatest(outstanding_not_after, $1) WHERE id = 1`, deadline)
		return err
	})
	return deadline, err
}

func (s *database) agentAuthorityEpoch(ctx context.Context) (uint64, error) {
	var epoch uint64
	err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch)
	return epoch, err
}

func (s *database) advanceAgentAuthority(ctx context.Context, expected uint64) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var epoch uint64
		var deadline time.Time
		if err := tx.QueryRowContext(ctx, `SELECT epoch, outstanding_not_after FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&epoch, &deadline); err != nil {
			return err
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if epoch != expected || !reconciliation.CanTakeOver(now, deadline) {
			return errors.New("authority cutover fenced by epoch or outstanding grants")
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_authority SET epoch = epoch + 1 WHERE id = 1`)
		return err
	})
}

func (s *fleetPersistence) acknowledgeAgentDesired(ctx context.Context, ack *agentv1.DesiredStateAcknowledgement) error {
	var epoch uint64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch); err != nil {
		return err
	}
	if ack.GetAuthorityEpoch() != epoch {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	return s.sessions.Acknowledge(ack.GetAgentId(), ack.GetSessionId(), ack.GetAuthorityEpoch(), deliverycore.SyncVersions{
		Cursor:      ack.GetReconciliationCursor(),
		NodeConfig:  ack.GetNodeConfigVersion(),
		Credentials: ack.GetCredentialsVersion(),
		Replicas:    ack.GetReplicasVersion(),
	})
}

func stampAgentCommand(state *agentv1.DesiredNodeState, sessionID string, epoch uint64, deadline time.Time) {
	state.SessionId = sessionID
	state.AuthorityEpoch = epoch
	state.AuthorityNotAfter = timestamppb.New(deadline)
}

func stampAllocationDiff(diff *agentv1.AllocationDiff, sessionID string, epoch uint64, deadline time.Time) {
	diff.SessionId = sessionID
	diff.AuthorityEpoch = epoch
	diff.AuthorityNotAfter = timestamppb.New(deadline)
}

func stampNodeConfigUpdate(update *agentv1.NodeConfigUpdate, sessionID string, epoch uint64, deadline time.Time) {
	update.SessionId = sessionID
	update.AuthorityEpoch = epoch
	update.AuthorityNotAfter = timestamppb.New(deadline)
}

func stampPullCredentials(creds *agentv1.PullCredentialSet, sessionID string, epoch uint64, deadline time.Time) {
	creds.SessionId = sessionID
	creds.AuthorityEpoch = epoch
	creds.AuthorityNotAfter = timestamppb.New(deadline)
}

func stampReplicaEndpoints(replicas *agentv1.ReplicaEndpoints, sessionID string, epoch uint64, deadline time.Time) {
	replicas.SessionId = sessionID
	replicas.AuthorityEpoch = epoch
	replicas.AuthorityNotAfter = timestamppb.New(deadline)
}

type liveNotifications interface {
	Watch(string) (<-chan struct{}, func())
	Notify(string)
}

type notifier struct {
	live liveNotifications
}

func newNotifier(live liveNotifications) *notifier {
	return &notifier{live: live}
}

func (n *notifier) Watch(agentID string) (<-chan struct{}, func()) {
	if n == nil || n.live == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	ch, stop := n.live.Watch(agentID)
	slog.Info("watch registered", "agent_id", agentID)
	return ch, func() {
		stop()
		slog.Info("watch removed", "agent_id", agentID)
	}
}

func (n *notifier) Notify(agentID string) {
	if n == nil || n.live == nil {
		return
	}
	n.live.Notify(agentID)
}
