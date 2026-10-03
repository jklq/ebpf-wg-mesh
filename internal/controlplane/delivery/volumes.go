package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// Volume sizes are whole mebibytes so a loop image never ends mid-block.
const (
	MinVolumeSizeBytes    int64 = 64 << 20
	MaxVolumeSizeBytes    int64 = 4 << 40
	volumeSizeGranularity int64 = 1 << 20
)

// Agent volume condition phases.
const (
	volumePhaseReady     = "Ready"
	volumePhaseFull      = "Full"
	volumePhaseError     = "Error"
	volumePhaseDestroyed = "Destroyed"
	volumePhaseOrphaned  = "Orphaned"
)

func ValidateVolumeSize(sizeBytes int64) error {
	if sizeBytes < MinVolumeSizeBytes || sizeBytes > MaxVolumeSizeBytes {
		return fmt.Errorf("%w: size must be between %d MiB and %d GiB", ErrInvalidVolumeSize, MinVolumeSizeBytes>>20, MaxVolumeSizeBytes>>30)
	}
	if sizeBytes%volumeSizeGranularity != 0 {
		return fmt.Errorf("%w: size must be a whole number of MiB", ErrInvalidVolumeSize)
	}
	return nil
}

// VolumeObservation is one agent's latest report for a volume on its disk.
type VolumeObservation struct {
	VolumeID      string
	AgentID       string
	Phase         string
	Message       string
	UsedBytes     int64
	CapacityBytes int64
	ObservedAt    time.Time
}

type volumeObsKey struct{ AgentID, VolumeID string }

// recordVolumeObservations replaces an agent's volume observations with its
// latest report. Each report covers every volume on the agent's disk.
func (l *Live) recordVolumeObservations(agentID, sessionID string, conditions []*agentv1.VolumeCondition, now time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.requireAccepting(); err != nil {
		return false, err
	}
	session, ok := l.sessions[agentID]
	if !ok || session.SessionID != sessionID {
		return false, ErrStaleAgentSession
	}
	changed := false
	seen := make(map[string]bool, len(conditions))
	for _, cond := range conditions {
		id := cond.GetVolumeId()
		if id == "" || cond.GetPhase() == volumePhaseDestroyed {
			continue
		}
		seen[id] = true
		key := volumeObsKey{AgentID: agentID, VolumeID: id}
		next := VolumeObservation{VolumeID: id, AgentID: agentID, Phase: cond.GetPhase(), Message: cond.GetMessage(),
			UsedBytes: cond.GetUsedBytes(), CapacityBytes: cond.GetCapacityBytes(), ObservedAt: now}
		previous, exists := l.volumeObservations[key]
		l.volumeObservations[key] = next
		if !exists || previous.Phase != next.Phase || previous.Message != next.Message ||
			previous.UsedBytes != next.UsedBytes || previous.CapacityBytes != next.CapacityBytes {
			changed = true
		}
	}
	for key := range l.volumeObservations {
		if key.AgentID == agentID && !seen[key.VolumeID] {
			delete(l.volumeObservations, key)
			changed = true
		}
	}
	return changed, nil
}

// OrphanedVolumes lists volume IDs an agent retains on disk without a desired
// entry, for operator review.
func (l *Live) OrphanedVolumes(agentID string) []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for key, obs := range l.volumeObservations {
		if key.AgentID == agentID && obs.Phase == volumePhaseOrphaned {
			out = append(out, key.VolumeID)
		}
	}
	slices.Sort(out)
	return out
}

// RenderVolumeStatus folds the pinned agent's latest report with its
// presence and administration. Off the live owner, only placement is known.
func (l *Live) RenderVolumeStatus(rec VolumeRecord) VolumeRecord {
	if rec.AgentID == "" {
		rec.Status = VolumeStatus{State: platformv1.VolumeState_VOLUME_STATE_PENDING}
		return rec
	}
	if l == nil {
		return rec
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	agent, known := l.agentLocked(rec.AgentID)
	if known {
		rec.AgentName = agent.Name
	}
	if !l.serving {
		return rec
	}
	nodeName := rec.AgentName
	if nodeName == "" {
		nodeName = rec.AgentID
	}
	obs, observed := l.volumeObservations[volumeObsKey{AgentID: rec.AgentID, VolumeID: rec.ID}]
	if observed {
		rec.Status.UsedBytes = obs.UsedBytes
		rec.Status.ObservedAt = obs.ObservedAt
	}
	switch {
	case !known || agent.LifecycleState == AgentStateRetired:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_UNAVAILABLE
		rec.Status.Message = fmt.Sprintf("Node %s is retired. The volume's data is not available on any other node.", nodeName)
	case agent.LifecycleState == AgentStateUnavailable:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_UNAVAILABLE
		rec.Status.Message = fmt.Sprintf("Node %s is unreachable. The service waits for it rather than starting elsewhere with an empty volume.", nodeName)
	case !observed || obs.Phase == volumePhaseOrphaned:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_PENDING
		rec.Status.Message = fmt.Sprintf("Waiting for node %s to report the volume.", nodeName)
	case obs.Phase == volumePhaseReady:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_READY
	case obs.Phase == volumePhaseFull:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_FULL
		rec.Status.Message = "The volume is full. Writes fail until it is grown."
	default:
		rec.Status.State = platformv1.VolumeState_VOLUME_STATE_ERROR
		rec.Status.Message = obs.Message
	}
	return rec
}

// recordVolumeReport folds an agent's volume conditions into live state and
// retires destructions the agent has completed. It reports whether rendered
// volume status changed.
func (d *Delivery) recordVolumeReport(ctx context.Context, agentID string, report *agentv1.StatusReport, durable journal.DurableState, now time.Time) (bool, error) {
	changed, err := d.live.recordVolumeObservations(agentID, report.GetSessionId(), report.GetVolumes(), now)
	if err != nil {
		return false, err
	}
	var completed []string
	for _, cond := range report.GetVolumes() {
		if cond.GetPhase() != volumePhaseDestroyed {
			continue
		}
		if destruction, ok := durable.Destructions[cond.GetVolumeId()]; ok && destruction.AgentID == agentID {
			completed = append(completed, cond.GetVolumeId())
		}
	}
	if len(completed) == 0 {
		return changed, nil
	}
	err = d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, volumeID := range completed {
			if _, err := journal.DestructionRow(volumeID).Exec(ctx, tx,
				`DELETE FROM volume_destructions WHERE volume_id = $1 AND agent_id = $2`, volumeID, agentID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("retire volume destructions: %w", err)
	}
	return true, nil
}

// RequestVolumeDestructionsTx turns volumes that are about to be hard-deleted
// into destroy instructions for their pinned agents. Callers pass the WHERE
// clause over the volumes table that selects the rows they delete.
func RequestVolumeDestructionsTx(ctx context.Context, tx *sql.Tx, now time.Time, where string, args ...any) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, agent_id FROM volumes WHERE agent_id IS NOT NULL AND (`+where+`)`, args...)
	if err != nil {
		return err
	}
	type pinned struct{ volumeID, agentID string }
	var targets []pinned
	for rows.Next() {
		var target pinned
		if err := rows.Scan(&target.volumeID, &target.agentID); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, target)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, target := range targets {
		if _, err := journal.DestructionRow(target.volumeID).Exec(ctx, tx,
			`INSERT INTO volume_destructions(volume_id, agent_id, requested_at) VALUES ($1, $2, $3)
			 ON CONFLICT (volume_id) DO NOTHING`, target.volumeID, target.agentID, now); err != nil {
			return err
		}
	}
	return nil
}

// growVolumeTx raises a volume's size. The pinned agent grows the filesystem in
// place; shrinking is rejected because it could discard data.
func (d *Delivery) growVolumeTx(ctx context.Context, tx *sql.Tx, volumeID string, sizeBytes int64) (VolumeRecord, error) {
	if err := ValidateVolumeSize(sizeBytes); err != nil {
		return VolumeRecord{}, err
	}
	var current int64
	var deletedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT size_bytes, deleted_at FROM volumes WHERE id = $1 FOR UPDATE`, volumeID).Scan(&current, &deletedAt); err != nil {
		return VolumeRecord{}, err
	}
	if deletedAt.Valid {
		return VolumeRecord{}, fmt.Errorf("%w: volume is deleted", ErrVolumeNotFound)
	}
	if sizeBytes < current {
		return VolumeRecord{}, fmt.Errorf("%w: %d MiB is smaller than the current %d MiB", ErrVolumeShrink, sizeBytes>>20, current>>20)
	}
	if sizeBytes != current {
		if _, err := journal.VolumeRow(volumeID).Exec(ctx, tx, `UPDATE volumes SET size_bytes = $1 WHERE id = $2`, sizeBytes, volumeID); err != nil {
			return VolumeRecord{}, err
		}
	}
	return ScanVolumeRow(tx.QueryRowContext(ctx, VolumeSelect+` WHERE v.id = $1`, volumeID))
}

// VolumeSelect reads a volume row with its effective deletion state.
const VolumeSelect = `SELECT v.id, v.environment_id, v.name, v.size_bytes, COALESCE(v.agent_id, ''), v.staged, v.created_at,
		v.deleted_at, v.deleted_by_user_id, v.delete_expires_at
	  FROM volumes v`

func ScanVolumeRow(row interface{ Scan(...any) error }) (VolumeRecord, error) {
	var rec VolumeRecord
	var tombstone Tombstone
	if err := row.Scan(&rec.ID, &rec.EnvironmentID, &rec.Name, &rec.SizeBytes, &rec.AgentID, &rec.Staged, &rec.CreatedAt,
		&tombstone.DeletedAt, &tombstone.DeletedByUserID, &tombstone.ExpiresAt); err != nil {
		return VolumeRecord{}, err
	}
	rec.Name = strings.TrimSpace(rec.Name)
	rec.Deletion = EffectiveDeletion(tombstone)
	return rec, nil
}

// GrowVolume raises a volume's size for a project writer.
func (d *Delivery) GrowVolume(ctx context.Context, user authz.User, volumeID string, sizeBytes int64) (VolumeRecord, error) {
	scope, err := d.store.authz.AuthorizeVolume(ctx, user, volumeID, authz.Write)
	if err != nil {
		return VolumeRecord{}, err
	}
	var rec VolumeRecord
	err = d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		rec, err = d.growVolumeTx(ctx, tx, scope.ID(), sizeBytes)
		return err
	})
	if err != nil {
		return VolumeRecord{}, err
	}
	return d.RenderVolumeStatus(rec), nil
}

// RenderVolumeStatus attaches live node status to a volume read.
func (d *Delivery) RenderVolumeStatus(rec VolumeRecord) VolumeRecord {
	if d == nil {
		return rec
	}
	return d.live.RenderVolumeStatus(rec)
}

// OrphanedVolumes lists volumes an agent retains without a desired entry.
func (d *Delivery) OrphanedVolumes(agentID string) []string {
	if d == nil {
		return nil
	}
	return d.live.OrphanedVolumes(agentID)
}

// DiscardStagedVolumeTx removes a never-released volume. It was never placed,
// so no node holds data for it and the row can go without a tombstone.
func DiscardStagedVolumeTx(ctx context.Context, tx *sql.Tx, volumeID string) (bool, error) {
	result, err := journal.VolumeRow(volumeID).Exec(ctx, tx,
		`DELETE FROM volumes WHERE id = $1 AND staged AND agent_id IS NULL`, volumeID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// discardUnmountedStagedVolumeTx follows a discarded draft: the staged volume
// it mounted goes with it unless another live service still references it.
func discardUnmountedStagedVolumeTx(ctx context.Context, tx *sql.Tx, environmentID, volumeName string) error {
	if volumeName == "" {
		return nil
	}
	var volumeID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM volumes
		  WHERE environment_id = $1 AND name = $2 AND staged AND deleted_at IS NULL
		  FOR UPDATE`, environmentID, volumeName).Scan(&volumeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	owner, err := VolumeAttachmentOwner(ctx, tx, environmentID, volumeName, "")
	if err != nil || owner != "" {
		return err
	}
	_, err = DiscardStagedVolumeTx(ctx, tx, volumeID)
	return err
}

// commitStagedVolumesTx makes an environment release commit the volumes
// created since the previous one.
func commitStagedVolumesTx(ctx context.Context, tx *sql.Tx, environmentID string) error {
	return journal.UpdateRows(ctx, tx, journal.TableVolumes,
		`UPDATE volumes SET staged = false
		  WHERE environment_id = $1 AND staged AND deleted_at IS NULL
		  RETURNING id`, environmentID)
}
