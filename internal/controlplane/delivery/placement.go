package delivery

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strconv"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/journal"
)

func (d *Delivery) reconcileServiceReplicasTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, preferredAgentID string, now time.Time) ([]AllocationRecord, error) {
	s := d.store
	desired := service.DesiredReplicaCount
	if err := validateVolumeReplicaCompatibility(service.Spec, desired); err != nil {
		return nil, err
	}
	if desired <= 0 && service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return nil, nil
	}
	existing, err := s.listAllocationsByServiceIDQuerier(ctx, tx, service.ID, true)
	if err != nil {
		return nil, err
	}
	if service.RolloutGeneration == 0 {
		if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
			return nil, err
		}
		return existing, nil
	}

	live, lost := splitLostAllocations(existing)
	existing = live

	if int32(len(existing)) > desired {
		removed, remaining := selectAllocationsToRemove(existing, int(desired))
		for _, alloc := range removed {
			if err := d.applyAllocationMutationsTx(ctx, tx, now, allocationMutation{Kind: mutationCompleteDrain, AllocationID: alloc.ID}); err != nil {
				return nil, err
			}
		}
		existing = remaining
	}

	if int32(len(existing)) < desired {
		needed := int(desired) - len(existing)
		occupied := make(map[string]struct{}, len(existing))
		for _, alloc := range existing {
			occupied[alloc.AgentID] = struct{}{}
		}
		placed := 0
		failureReason := "no active healthy node satisfies the placement constraints"
		for i := 0; i < needed; i++ {
			agentID, err := d.chooseReplicaAgentTx(ctx, tx, service, occupied, preferredAgentID, i == 0 && preferredAgentID != "")
			if errors.Is(err, ErrNoPlacementAvailable) {
				failureReason = strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrNoPlacementAvailable.Error()+": "))
				break
			}
			if err != nil {
				return nil, err
			}
			alloc, err := d.insertAllocationTx(ctx, tx, service, agentID, now)
			if err != nil {
				return nil, err
			}
			existing = append(existing, alloc)
			occupied[agentID] = struct{}{}
			placed++
			preferredAgentID = ""
		}
		if placed < needed {
			message := pendingPlacementMessage(len(existing), int(desired), failureReason)
			if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, message, now); err != nil {
				return nil, err
			}
			service.PlacementMessage = message
			return append(existing, lost...), nil
		}
	}

	if err := s.setServicePlacementMessageTx(ctx, tx, service.ID, "", now); err != nil {
		return nil, err
	}
	service.PlacementMessage = ""
	return append(existing, lost...), nil
}

func (d *Delivery) chooseReplicaAgentTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, occupied map[string]struct{}, preferredAgentID string, usePreferred bool) (string, error) {
	s := d.store
	if volumeName := ServiceVolumeName(service.Spec); volumeName != "" {
		return d.chooseVolumeAgentTx(ctx, tx, service, volumeName)
	}
	if usePreferred && preferredAgentID != "" {
		if _, taken := occupied[preferredAgentID]; !taken {
			candidates, err := s.placementCandidatesQuerier(ctx, tx)
			if err != nil {
				return "", err
			}
			for _, candidate := range candidates {
				if candidate.ID == preferredAgentID && candidateEligible(candidate, service.Spec) {
					return preferredAgentID, nil
				}
			}
		}
	}
	return d.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, occupied)
}

// chooseVolumeAgentTx places a volume-backed service on the node holding its
// volume. A volume that has never been placed is pinned to the node chosen for
// its first allocation, in the same transaction; it never moves afterwards.
func (d *Delivery) chooseVolumeAgentTx(ctx context.Context, tx *sql.Tx, service ServiceRecord, volumeName string) (string, error) {
	s := d.store
	var volumeID string
	var pinned sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, agent_id FROM volumes
		WHERE environment_id = $1 AND name = $2 AND deleted_at IS NULL FOR UPDATE`,
		service.EnvironmentID, volumeName).Scan(&volumeID, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrVolumeNotFound, volumeName)
	}
	if err != nil {
		return "", err
	}
	candidates, err := s.placementCandidatesQuerier(ctx, tx)
	if err != nil {
		return "", err
	}
	if pinned.Valid && pinned.String != "" {
		for _, candidate := range candidates {
			if candidate.ID != pinned.String {
				continue
			}
			if !candidateEligible(candidate, service.Spec) {
				return "", fmt.Errorf("%w: volume %q is on node %s, which lacks capacity or does not match the placement region", ErrNoPlacementAvailable, volumeName, candidate.ID)
			}
			return candidate.ID, nil
		}
		return "", fmt.Errorf("%w: %s", ErrNoPlacementAvailable, s.volumeNodeUnavailableReason(ctx, tx, volumeName, pinned.String))
	}
	agentID, err := d.chooseAgentForReplicaQuerier(ctx, tx, service.Spec, map[string]struct{}{})
	if err != nil {
		return "", err
	}
	if _, err := journal.VolumeRow(volumeID).Exec(ctx, tx,
		`UPDATE volumes SET agent_id = $1, staged = false WHERE id = $2 AND agent_id IS NULL`, agentID, volumeID); err != nil {
		return "", err
	}
	return agentID, nil
}

// volumeNodeUnavailableReason explains why a pinned volume's node cannot run
// its service. The service never starts elsewhere with an empty volume.
func (s *persistence) volumeNodeUnavailableReason(ctx context.Context, q ServiceQueryer, volumeName, agentID string) string {
	var name, state string
	if err := q.QueryRowContext(ctx, `SELECT name, lifecycle_state FROM agents WHERE id = $1`, agentID).Scan(&name, &state); err != nil {
		return fmt.Sprintf("volume %q is on node %s, which is unavailable", volumeName, agentID)
	}
	switch AgentLifecycleState(state) {
	case AgentStateRetired:
		return fmt.Sprintf("volume %q is on node %s, which is retired; its data is not available on any other node", volumeName, name)
	case AgentStateActive:
		return fmt.Sprintf("volume %q is on node %s, which is not reachable", volumeName, name)
	default:
		return fmt.Sprintf("volume %q is on node %s, which is %s", volumeName, name, state)
	}
}

func (d *Delivery) chooseAgentForReplicaQuerier(ctx context.Context, q ServiceQueryer, spec *platformv1.ServiceSpec, occupied map[string]struct{}) (string, error) {
	s := d.store
	candidates, err := s.placementCandidatesQuerier(ctx, q)
	if err != nil {
		return "", err
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, true); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, true, false); agentID != "" {
		return agentID, nil
	}
	if agentID := firstEligibleReplicaAgent(candidates, spec, s.reservedAgentIDs, occupied, false, false); agentID != "" {
		return agentID, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoPlacementAvailable, placementFailureReason(candidates, spec, s.reservedAgentIDs))
}

type placementCandidate struct {
	HostType               config.HostType
	ID                     string
	Region                 string
	Zone                   string
	FailureDomain          string
	RuntimeCapabilities    []string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func (s *persistence) placementCandidatesQuerier(ctx context.Context, q ServiceQueryer) ([]placementCandidate, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.region, r.zone, r.failure_domain, r.runtime_capabilities, ad.host_type,
		greatest(r.cpu_millis_capacity - r.reserved_cpu_millis, 0),
		greatest(r.memory_mebibytes_capacity - r.reserved_memory_mebibytes, 0),
		COALESCE(stats.service_count, 0), COALESCE(stats.cpu_millis, 0), COALESCE(stats.memory_mebibytes, 0)
		FROM agent_registrations r
		JOIN agent_administration ad ON ad.agent_id = r.id
		LEFT JOIN (
			SELECT a.agent_id, COUNT(*) AS service_count,
				COALESCE(SUM(COALESCE((rev.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
				COALESCE(SUM(COALESCE((rev.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
			FROM services svc
			JOIN allocation_assignments a ON a.service_id = svc.id
			JOIN service_revisions rev ON rev.service_id = svc.id AND rev.spec_revision = svc.current_spec_revision
			WHERE a.rollout_state <> 'lost'
			GROUP BY a.agent_id
		) stats ON stats.agent_id = r.id
		WHERE ad.lifecycle_state = 'active'
		ORDER BY CASE ad.host_type WHEN 'stable' THEN 0 ELSE 1 END, COALESCE(stats.service_count, 0), r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []placementCandidate
	for rows.Next() {
		var candidate placementCandidate
		if err := rows.Scan(&candidate.ID, &candidate.Region, &candidate.Zone, &candidate.FailureDomain,
			(*jsonStringSlice)(&candidate.RuntimeCapabilities), &candidate.HostType, &candidate.CPUMillisCapacity,
			&candidate.MemoryMebibytesCapcity, &candidate.ServiceCount, &candidate.UsedCPUMillis,
			&candidate.UsedMemoryMebibytes); err != nil {
			return nil, err
		}
		if s.live != nil && s.live.Admitted(candidate.ID) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates, rows.Err()
}

func IPv4SubnetAt(poolCIDR string, prefixBits int, ordinal uint64) (string, error) {
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil || !pool.Addr().Is4() {
		return "", fmt.Errorf("parse IPv4 workload pool %q", poolCIDR)
	}
	if pool != pool.Masked() {
		return "", fmt.Errorf("IPv4 workload pool %q must be canonical", poolCIDR)
	}
	pool = pool.Masked()
	if prefixBits <= pool.Bits() || prefixBits > 30 || prefixBits-pool.Bits() > 31 {
		return "", fmt.Errorf("invalid IPv4 child prefix /%d for pool %q", prefixBits, poolCIDR)
	}
	count := uint64(1) << uint(prefixBits-pool.Bits())
	if ordinal >= count {
		return "", fmt.Errorf("IPv4 workload pool %q exhausted", poolCIDR)
	}
	baseBytes := pool.Addr().As4()
	base := binary.BigEndian.Uint32(baseBytes[:])
	base += uint32(ordinal) << uint(32-prefixBits)
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], base)
	return netip.PrefixFrom(netip.AddrFrom4(out), prefixBits).String(), nil
}

func nextIPv4AddressFromSubnet(subnetCIDR string, used map[string]struct{}) (string, error) {
	subnet, err := netip.ParsePrefix(subnetCIDR)
	if err != nil || !subnet.Addr().Is4() {
		return "", fmt.Errorf("parse IPv4 workload subnet %q", subnetCIDR)
	}
	subnet = subnet.Masked()
	if subnet.Bits() > 30 {
		return "", fmt.Errorf("IPv4 workload subnet %q is too small", subnetCIDR)
	}
	baseBytes := subnet.Addr().As4()
	base := binary.BigEndian.Uint32(baseBytes[:])
	size := uint64(1) << uint(32-subnet.Bits())
	for offset := uint64(2); offset+1 < size; offset++ {
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], base+uint32(offset))
		addr := netip.AddrFrom4(raw).String()
		if _, exists := used[addr]; !exists {
			return addr, nil
		}
	}
	return "", fmt.Errorf("IPv4 workload subnet %q exhausted", subnetCIDR)
}

func IPv4PrefixesOverlap(left, right netip.Prefix) bool {
	return left.Contains(right.Addr()) || right.Contains(left.Addr())
}

func privateIPv6(subnetCIDR, environmentID, allocationID string) (string, error) {
	prefix, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return "", fmt.Errorf("parse workload subnet %q: %w", subnetCIDR, err)
	}
	if !prefix.Addr().Is6() {
		return "", fmt.Errorf("workload subnet %q is not IPv6", subnetCIDR)
	}
	if bits := prefix.Bits(); bits != 64 {
		return "", fmt.Errorf("workload subnet %q must be /64", subnetCIDR)
	}
	base := prefix.Masked().Addr().As16()
	sum := sha1.Sum([]byte(environmentID + ":" + allocationID))
	copy(base[8:], sum[:8])
	base[15] = 0x10
	return netip.AddrFrom16(base).String(), nil
}

func nextSubnetFromPool(poolCIDR string, prefixBits int, used map[string]struct{}, agentID string) (string, error) {
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool %q: %w", poolCIDR, err)
	}
	if !pool.Addr().Is6() {
		return "", fmt.Errorf("pool %q is not IPv6", poolCIDR)
	}
	if prefixBits < pool.Bits() || prefixBits > 128 {
		return "", fmt.Errorf("invalid child prefix /%d for pool %q", prefixBits, poolCIDR)
	}

	base := pool.Masked().Addr().As16()
	max := 1 << (prefixBits - pool.Bits())
	start := preferredOrdinal(agentID, max-1)
	for offset := 0; offset < max-1; offset++ {
		i := ((start + offset - 1) % (max - 1)) + 1
		addr := base
		writeSubnetBits(addr[:], pool.Bits(), prefixBits, i)
		prefix := netip.PrefixFrom(netip.AddrFrom16(addr), prefixBits).Masked().String()
		if _, exists := used[prefix]; !exists {
			return prefix, nil
		}
	}
	return "", fmt.Errorf("pool %q exhausted", poolCIDR)
}

func nextAddressFromPool(poolCIDR string, used map[string]struct{}, hostOffset uint16, agentID string) (string, error) {
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil {
		return "", fmt.Errorf("parse pool %q: %w", poolCIDR, err)
	}
	if !pool.Addr().Is6() {
		return "", fmt.Errorf("pool %q is not IPv6", poolCIDR)
	}
	if pool.Bits() > 112 {
		return "", fmt.Errorf("pool %q too small for sequential node addresses", poolCIDR)
	}

	base := pool.Masked().Addr().As16()
	start := uint16(preferredOrdinal(agentID, 65535-int(hostOffset)-1))
	for offset := uint16(0); offset < 65535-hostOffset-1; offset++ {
		i := ((start + offset - 1) % (65535 - hostOffset - 1)) + 1
		addr := base
		binaryBigPutUint16(addr[14:], hostOffset+i)
		cidr := netip.PrefixFrom(netip.AddrFrom16(addr), pool.Bits()).String()
		if _, exists := used[cidr]; !exists {
			return cidr, nil
		}
	}
	return "", fmt.Errorf("pool %q exhausted", poolCIDR)
}

func writeSubnetBits(dst []byte, startBits, endBits, value int) {
	for bit := startBits; bit < endBits; bit++ {
		byteIndex := bit / 8
		bitIndex := 7 - (bit % 8)
		mask := byte(1 << bitIndex)
		if value&(1<<(endBits-bit-1)) != 0 {
			dst[byteIndex] |= mask
			continue
		}
		dst[byteIndex] &^= mask
	}
}

func binaryBigPutUint16(dst []byte, value uint16) {
	if len(dst) < 2 {
		return
	}
	dst[0] = byte(value >> 8)
	dst[1] = byte(value)
}

func preferredOrdinal(agentID string, max int) int {
	if max <= 0 {
		return 1
	}
	if suffix := trailingNumber(agentID); suffix > 0 {
		return ((suffix - 1) % max) + 1
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(agentID))
	return int(hasher.Sum32()%uint32(max)) + 1
}

func trailingNumber(value string) int {
	end := len(value)
	start := end
	for start > 0 {
		if value[start-1] < '0' || value[start-1] > '9' {
			break
		}
		start--
	}
	if start == end {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSpace(value[start:end]))
	if err != nil {
		return 0
	}
	return number
}
