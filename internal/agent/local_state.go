package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

const (
	localStateFormatVersion uint64 = 1
	localStateFileName             = "agent-state.db"
)

var (
	errLocalStateCorrupt = errors.New("local state is corrupt")

	localMetaBucket         = []byte("meta")
	localDesiredBucket      = []byte("desired")
	localCredentialsBucket  = []byte("credentials")
	localAllocationsBucket  = []byte("allocations")
	localObservationsBucket = []byte("observations")
	localInventoryBucket    = []byte("runtime_inventory")

	formatVersionKey       = []byte("format_version")
	initStateKey           = []byte("initialization_state")
	agentIdentityKey       = []byte("agent_identity")
	clusterIdentityKey     = []byte("cluster_identity")
	authorityEpochKey      = []byte("accepted_authority_epoch")
	cursorKey              = []byte("reconciliation_cursor")
	desiredStateKey        = []byte("accepted_state")
	statusReportKey        = []byte("status_report")
	reportEpochKey         = []byte("report_authority_epoch")
	reportCursorKey        = []byte("report_reconciliation_cursor")
	observationSequenceKey = []byte("observation_sequence")
)

type initializationState string

const (
	initializationUninitialized initializationState = "uninitialized"
	initializationReady         initializationState = "ready"
	initializationRecovery      initializationState = "recovery"
)

// RuntimeResource is an allocation- or volume-derived identity discovered from
// the runtime independently of local desired-state records.
type RuntimeResource struct {
	AllocationID string `json:"allocation_id"`
	VolumeID     string `json:"volume_id"`
	RuntimeID    string `json:"runtime_id"`
}

type localAllocationState struct {
	AllocationID        string `json:"allocation_id"`
	RuntimeID           string `json:"runtime_id"`
	DesiredSpecRevision int64  `json:"desired_spec_revision"`
	AppliedSpecRevision int64  `json:"applied_spec_revision"`
	DesiredGeneration   int64  `json:"desired_generation"`
	AppliedGeneration   int64  `json:"applied_generation"`
	PendingOperation    string `json:"pending_operation,omitempty"`
	DrainDeadline       string `json:"drain_deadline,omitempty"`
	Terminal            bool   `json:"terminal"`
	Phase               string `json:"phase,omitempty"`
}

type pullCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type localStateSummary struct {
	Initialization       initializationState
	AgentIdentity        string
	ClusterIdentity      string
	AuthorityEpoch       uint64
	ReconciliationCursor int64
	RuntimeResources     []RuntimeResource
	QuarantinedStore     string
}

// localStateStore is the durable seam between transport and supervision. A
// desired snapshot, its cursor, allocation operation state, and observations
// are committed atomically. Ephemeral pull credentials live in a separate
// bucket so renewing them does not manufacture an allocation change.
type localStateStore struct {
	path        string
	db          *bbolt.DB
	quarantined string
}

func openLocalStateStore(dataDir, agentID string) (*localStateStore, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("local state requires an agent identity")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create agent data directory: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure agent data directory: %w", err)
	}
	path := filepath.Join(dataDir, localStateFileName)
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat local state: %w", statErr)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil && existed && isBoltCorruption(err) {
		return quarantineLocalState(path, agentID, db, err)
	}
	if errors.Is(err, bbolt.ErrTimeout) {
		return nil, fmt.Errorf("open local state: database is locked by another agent process")
	}
	if err != nil {
		return nil, fmt.Errorf("open local state: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure local state: %w", err)
	}
	store := &localStateStore{path: path, db: db}
	if err := store.initialize(agentID, existed, false); err != nil {
		if errors.Is(err, errLocalStateCorrupt) {
			return quarantineLocalState(path, agentID, db, err)
		}
		_ = db.Close()
		return nil, err
	}
	if err := store.validateRecords(); err != nil {
		return quarantineLocalState(path, agentID, db, err)
	}
	return store, nil
}

func quarantineLocalState(path, agentID string, db *bbolt.DB, cause error) (*localStateStore, error) {
	if db != nil {
		if err := db.Close(); err != nil {
			return nil, errors.Join(cause, err)
		}
	}
	quarantined := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(path, quarantined); err != nil {
		return nil, fmt.Errorf("quarantine corrupt local state after %v: %w", cause, err)
	}
	recreated, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("recreate local state after corruption: %w", err)
	}
	store := &localStateStore{path: path, db: recreated, quarantined: quarantined}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = recreated.Close()
		return nil, fmt.Errorf("secure recreated local state: %w", err)
	}
	if err := store.initialize(agentID, false, true); err != nil {
		_ = recreated.Close()
		return nil, err
	}
	return store, nil
}

func isBoltCorruption(err error) bool {
	return errors.Is(err, bbolt.ErrInvalid) || errors.Is(err, bbolt.ErrVersionMismatch) || errors.Is(err, bbolt.ErrChecksum)
}

func (s *localStateStore) initialize(agentID string, existed, corrupt bool) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(localMetaBucket)
		if err != nil {
			return err
		}
		for _, bucket := range [][]byte{localDesiredBucket, localCredentialsBucket, localAllocationsBucket, localObservationsBucket, localInventoryBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		rawVersion := meta.Get(formatVersionKey)
		if len(rawVersion) != 0 && len(rawVersion) != 8 {
			return fmt.Errorf("%w: invalid format version encoding", errLocalStateCorrupt)
		}
		if version := readUint64(rawVersion); version != 0 && version != localStateFormatVersion {
			return fmt.Errorf("unsupported local state format %d (expected %d)", version, localStateFormatVersion)
		}
		if existing := string(meta.Get(agentIdentityKey)); existing != "" && existing != agentID {
			return fmt.Errorf("local state belongs to agent %q, configured identity is %q", existing, agentID)
		}
		if err := putUint64(meta, formatVersionKey, localStateFormatVersion); err != nil {
			return err
		}
		if err := meta.Put(agentIdentityKey, []byte(agentID)); err != nil {
			return err
		}
		if len(meta.Get(initStateKey)) == 0 {
			state := initializationUninitialized
			if corrupt || existed {
				state = initializationRecovery
			}
			return meta.Put(initStateKey, []byte(state))
		}
		return nil
	})
}

func (s *localStateStore) validateRecords() error {
	return s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		for _, key := range [][]byte{authorityEpochKey, cursorKey} {
			if err := validateIntegerEncoding(meta, key); err != nil {
				return err
			}
		}
		state := initializationState(meta.Get(initStateKey))
		if state != initializationUninitialized && state != initializationReady && state != initializationRecovery {
			return fmt.Errorf("invalid local initialization state %q", state)
		}
		agentID := string(meta.Get(agentIdentityKey))
		if strings.TrimSpace(agentID) == "" {
			return errors.New("local agent identity is empty")
		}
		desiredAllocations := make(map[string]struct{})
		if raw := tx.Bucket(localDesiredBucket).Get(desiredStateKey); len(raw) > 0 {
			var desired agentv1.DesiredNodeState
			if err := proto.Unmarshal(raw, &desired); err != nil {
				return fmt.Errorf("decode accepted desired state: %w", err)
			}
			if err := validateDesiredState(&desired); err != nil {
				return fmt.Errorf("validate accepted desired state: %w", err)
			}
			if desired.GetAgentId() != agentID || desired.GetAuthorityEpoch() != readUint64(meta.Get(authorityEpochKey)) || desired.GetReconciliationCursor() != readInt64(meta.Get(cursorKey)) {
				return errors.New("accepted desired state does not match local identity or reconciliation position")
			}
			for _, service := range desired.GetServices() {
				desiredAllocations[service.GetAllocationId()] = struct{}{}
			}
		} else if readUint64(meta.Get(authorityEpochKey)) != 0 || readInt64(meta.Get(cursorKey)) != 0 {
			return errors.New("local reconciliation position exists without desired state")
		}
		if err := tx.Bucket(localCredentialsBucket).ForEach(func(key, value []byte) error {
			allocationID := string(key)
			if _, ok := desiredAllocations[allocationID]; !ok {
				return fmt.Errorf("pull credential references unknown allocation %q", allocationID)
			}
			var credential pullCredential
			if err := json.Unmarshal(value, &credential); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("decode credential state: %w", err)
		}
		if err := tx.Bucket(localAllocationsBucket).ForEach(func(key, value []byte) error {
			var allocation localAllocationState
			if err := json.Unmarshal(value, &allocation); err != nil {
				return err
			}
			if allocation.AllocationID != string(key) {
				return fmt.Errorf("allocation key %q does not match record %q", key, allocation.AllocationID)
			}
			return validateRuntimeID("allocation ID", allocation.AllocationID)
		}); err != nil {
			return fmt.Errorf("decode allocation state: %w", err)
		}
		if err := tx.Bucket(localInventoryBucket).ForEach(func(key, value []byte) error {
			var resource RuntimeResource
			if err := json.Unmarshal(value, &resource); err != nil {
				return err
			}
			expected, err := runtimeResourceKey(resource)
			if err != nil {
				return err
			}
			if !bytes.Equal(key, expected) {
				return fmt.Errorf("runtime inventory key %q does not match resource %q", key, expected)
			}
			if strings.TrimSpace(resource.RuntimeID) == "" {
				return errors.New("runtime inventory resource has no runtime identity")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("decode runtime inventory: %w", err)
		}
		observations := tx.Bucket(localObservationsBucket)
		for _, key := range [][]byte{observationSequenceKey, reportEpochKey, reportCursorKey} {
			if err := validateIntegerEncoding(observations, key); err != nil {
				return err
			}
		}
		if raw := observations.Get(statusReportKey); len(raw) > 0 {
			var report agentv1.StatusReport
			if err := proto.Unmarshal(raw, &report); err != nil {
				return fmt.Errorf("decode observation state: %w", err)
			}
			if report.GetAgentId() != agentID || report.GetObservationSequence() == 0 || report.GetObservationSequence() != readUint64(observations.Get(observationSequenceKey)) {
				return errors.New("status report does not match local identity or observation sequence")
			}
			if report.GetAuthorityEpoch() != readUint64(observations.Get(reportEpochKey)) || report.GetReconciliationCursor() != readInt64(observations.Get(reportCursorKey)) {
				return errors.New("status report does not match its reconciliation position")
			}
		}
		return nil
	})
}

func (s *localStateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *localStateStore) prepareStartup(clusterID string, inventory []RuntimeResource) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
		if string(meta.Get(clusterIdentityKey)) == "" && clusterID != "" {
			if err := meta.Put(clusterIdentityKey, []byte(clusterID)); err != nil {
				return err
			}
		}
		state := initializationState(meta.Get(initStateKey))
		if state == initializationUninitialized {
			state = initializationReady
			if len(inventory) > 0 {
				state = initializationRecovery
			}
			if err := meta.Put(initStateKey, []byte(state)); err != nil {
				return err
			}
		}
		allocations := tx.Bucket(localAllocationsBucket)
		if err := writeRuntimeInventory(tx.Bucket(localInventoryBucket), inventory); err != nil {
			return err
		}
		if err := clearAllocationRuntimeIdentities(allocations); err != nil {
			return err
		}
		for _, resource := range inventory {
			if resource.AllocationID == "" {
				continue
			}
			current, err := readAllocation(allocations, resource.AllocationID)
			if err != nil {
				return err
			}
			current.AllocationID = resource.AllocationID
			current.RuntimeID = resource.RuntimeID
			if state == initializationRecovery && current.PendingOperation == "" {
				current.PendingOperation = "establish-ownership"
			}
			if err := writeAllocation(allocations, current); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *localStateStore) recordRuntimeInventory(inventory []RuntimeResource) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := writeRuntimeInventory(tx.Bucket(localInventoryBucket), inventory); err != nil {
			return err
		}
		allocations := tx.Bucket(localAllocationsBucket)
		if err := clearAllocationRuntimeIdentities(allocations); err != nil {
			return err
		}
		for _, resource := range inventory {
			if resource.AllocationID == "" {
				continue
			}
			allocation, err := readAllocation(allocations, resource.AllocationID)
			if err != nil {
				return err
			}
			allocation.AllocationID = resource.AllocationID
			allocation.RuntimeID = resource.RuntimeID
			if err := writeAllocation(allocations, allocation); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *localStateStore) acceptDesired(clusterID string, incoming *agentv1.DesiredNodeState) (bool, error) {
	if incoming == nil {
		return false, errors.New("desired state is nil")
	}
	if err := validateDesiredState(incoming); err != nil {
		return false, err
	}
	clean, credentials := splitDesiredCredentials(incoming)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clean)
	if err != nil {
		return false, fmt.Errorf("encode desired state: %w", err)
	}
	changed := false
	err = s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if incoming.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return fmt.Errorf("desired state agent %q does not match local identity %q", incoming.GetAgentId(), meta.Get(agentIdentityKey))
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
		clusterWasKnown := string(meta.Get(clusterIdentityKey)) != ""
		if string(meta.Get(clusterIdentityKey)) == "" {
			if clusterID == "" {
				return errors.New("authenticated cluster identity is unavailable")
			}
			if err := meta.Put(clusterIdentityKey, []byte(clusterID)); err != nil {
				return err
			}
		}
		acceptedEpoch := readUint64(meta.Get(authorityEpochKey))
		acceptedCursor := readInt64(meta.Get(cursorKey))
		switch {
		case incoming.GetAuthorityEpoch() < acceptedEpoch:
			return fmt.Errorf("stale authority epoch %d follows %d", incoming.GetAuthorityEpoch(), acceptedEpoch)
		case incoming.GetAuthorityEpoch() == acceptedEpoch && incoming.GetReconciliationCursor() < acceptedCursor:
			return fmt.Errorf("stale reconciliation cursor %d follows %d", incoming.GetReconciliationCursor(), acceptedCursor)
		}
		desired := tx.Bucket(localDesiredBucket)
		previous := desired.Get(desiredStateKey)
		if incoming.GetAuthorityEpoch() == acceptedEpoch && incoming.GetReconciliationCursor() == acceptedCursor && len(previous) > 0 {
			var previousState agentv1.DesiredNodeState
			if err := proto.Unmarshal(previous, &previousState); err != nil {
				return fmt.Errorf("decode accepted desired state: %w", err)
			}
			if !desiredConfigurationEqual(&previousState, clean) {
				return fmt.Errorf("desired state changed without advancing reconciliation cursor %d", acceptedCursor)
			}
		} else {
			changed = true
		}
		if err := desired.Put(desiredStateKey, encoded); err != nil {
			return err
		}
		if err := putUint64(meta, authorityEpochKey, incoming.GetAuthorityEpoch()); err != nil {
			return err
		}
		if err := putInt64(meta, cursorKey, incoming.GetReconciliationCursor()); err != nil {
			return err
		}
		if err := replaceCredentials(tx.Bucket(localCredentialsBucket), credentials); err != nil {
			return err
		}
		if err := updateDesiredAllocations(tx.Bucket(localAllocationsBucket), clean); err != nil {
			return err
		}
		if initializationState(meta.Get(initStateKey)) == initializationRecovery {
			// When the store or its cluster binding survived, an authenticated
			// complete snapshot from that same authority establishes ownership of
			// both present and absent allocation IDs. Without a prior cluster
			// binding, only an exact inventory claim can safely lift the fence.
			established := clusterWasKnown
			if !established {
				var err error
				established, err = recoveryOwnershipEstablished(tx.Bucket(localInventoryBucket), clean)
				if err != nil {
					return err
				}
			}
			if established {
				if err := meta.Put(initStateKey, []byte(initializationReady)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return changed, err
}

func validateDesiredState(state *agentv1.DesiredNodeState) error {
	if strings.TrimSpace(state.GetAgentId()) == "" {
		return errors.New("desired state agent_id is required")
	}
	if state.GetAuthorityEpoch() == 0 {
		return errors.New("desired state authority_epoch must be greater than zero")
	}
	if state.GetReconciliationCursor() < 0 {
		return errors.New("desired state reconciliation_cursor must not be negative")
	}
	if state.GetNodeConfig() == nil {
		return errors.New("desired state node_config is required")
	}
	services := make(map[string]struct{}, len(state.GetServices()))
	for _, service := range state.GetServices() {
		allocationID := service.GetAllocationId()
		if err := validateRuntimeID("allocation ID", allocationID); err != nil {
			return err
		}
		if _, exists := services[allocationID]; exists {
			return fmt.Errorf("duplicate desired allocation %q", allocationID)
		}
		services[allocationID] = struct{}{}
		if strings.TrimSpace(service.GetServiceId()) == "" || service.GetDesiredSpecRevision() <= 0 || service.GetDesiredRolloutGeneration() <= 0 {
			return fmt.Errorf("desired allocation %q has incomplete identity or generation", allocationID)
		}
	}
	volumes := make(map[string]struct{}, len(state.GetVolumes()))
	for _, volume := range state.GetVolumes() {
		volumeID := volume.GetVolumeId()
		if err := validateRuntimeID("volume ID", volumeID); err != nil {
			return err
		}
		if _, exists := volumes[volumeID]; exists {
			return fmt.Errorf("duplicate desired volume %q", volumeID)
		}
		volumes[volumeID] = struct{}{}
	}
	return nil
}

func (s *localStateStore) desiredState() (*agentv1.DesiredNodeState, error) {
	var state *agentv1.DesiredNodeState
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(localDesiredBucket).Get(desiredStateKey)
		if len(raw) == 0 {
			return nil
		}
		state = &agentv1.DesiredNodeState{}
		if err := proto.Unmarshal(raw, state); err != nil {
			return fmt.Errorf("decode desired state: %w", err)
		}
		credentials := tx.Bucket(localCredentialsBucket)
		for _, service := range state.GetServices() {
			if rawCredential := credentials.Get([]byte(service.GetAllocationId())); len(rawCredential) > 0 {
				var credential pullCredential
				if err := json.Unmarshal(rawCredential, &credential); err != nil {
					return fmt.Errorf("decode pull credential for %s: %w", service.GetAllocationId(), err)
				}
				service.RegistryUsername = credential.Username
				service.RegistryPassword = credential.Password
			}
		}
		return nil
	})
	return state, err
}

func (s *localStateStore) recordReport(report *agentv1.StatusReport) (*agentv1.StatusReport, bool, error) {
	if report == nil {
		return nil, false, errors.New("status report is nil")
	}
	var persisted *agentv1.StatusReport
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if report.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return fmt.Errorf("status report agent %q does not match local identity %q", report.GetAgentId(), meta.Get(agentIdentityKey))
		}
		epoch := readUint64(meta.Get(authorityEpochKey))
		cursor := readInt64(meta.Get(cursorKey))
		if epoch == 0 || len(tx.Bucket(localDesiredBucket).Get(desiredStateKey)) == 0 {
			return errors.New("cannot record observations before desired state is accepted")
		}
		next := proto.Clone(report).(*agentv1.StatusReport)
		next.SessionId = ""
		next.AuthorityEpoch = epoch
		next.ReconciliationCursor = cursor
		observations := tx.Bucket(localObservationsBucket)
		var previous *agentv1.StatusReport
		if raw := observations.Get(statusReportKey); len(raw) > 0 {
			previous = &agentv1.StatusReport{}
			if err := proto.Unmarshal(raw, previous); err != nil {
				return fmt.Errorf("decode status report: %w", err)
			}
		}
		sequence := readUint64(observations.Get(observationSequenceKey))
		if statusConfigurationEqual(previous, next) {
			next.ObservationSequence = sequence
		} else {
			if sequence >= math.MaxInt64 {
				return errors.New("observation sequence exhausted")
			}
			sequence++
			next.ObservationSequence = sequence
			changed = true
		}
		if sequence == 0 {
			sequence = 1
			next.ObservationSequence = sequence
			changed = true
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(next)
		if err != nil {
			return err
		}
		if err := observations.Put(statusReportKey, raw); err != nil {
			return err
		}
		if err := putUint64(observations, observationSequenceKey, sequence); err != nil {
			return err
		}
		if err := putUint64(observations, reportEpochKey, epoch); err != nil {
			return err
		}
		if err := putInt64(observations, reportCursorKey, cursor); err != nil {
			return err
		}
		if err := applyReportToAllocations(tx.Bucket(localAllocationsBucket), next); err != nil {
			return err
		}
		persisted = next
		return nil
	})
	return persisted, changed, err
}

func (s *localStateStore) currentReport() (*agentv1.StatusReport, error) {
	var report *agentv1.StatusReport
	err := s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		observations := tx.Bucket(localObservationsBucket)
		if readUint64(observations.Get(reportEpochKey)) != readUint64(meta.Get(authorityEpochKey)) ||
			readInt64(observations.Get(reportCursorKey)) != readInt64(meta.Get(cursorKey)) {
			return nil
		}
		raw := observations.Get(statusReportKey)
		if len(raw) == 0 {
			return nil
		}
		report = &agentv1.StatusReport{}
		return proto.Unmarshal(raw, report)
	})
	return report, err
}

func (s *localStateStore) summary() (localStateSummary, error) {
	result := localStateSummary{QuarantinedStore: s.quarantined}
	err := s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		result.Initialization = initializationState(meta.Get(initStateKey))
		result.AgentIdentity = string(meta.Get(agentIdentityKey))
		result.ClusterIdentity = string(meta.Get(clusterIdentityKey))
		result.AuthorityEpoch = readUint64(meta.Get(authorityEpochKey))
		result.ReconciliationCursor = readInt64(meta.Get(cursorKey))
		return tx.Bucket(localInventoryBucket).ForEach(func(_, value []byte) error {
			var resource RuntimeResource
			if err := json.Unmarshal(value, &resource); err != nil {
				return err
			}
			result.RuntimeResources = append(result.RuntimeResources, resource)
			return nil
		})
	})
	sort.Slice(result.RuntimeResources, func(i, j int) bool {
		left, _ := runtimeResourceKey(result.RuntimeResources[i])
		right, _ := runtimeResourceKey(result.RuntimeResources[j])
		return bytes.Compare(left, right) < 0
	})
	return result, err
}

func splitDesiredCredentials(state *agentv1.DesiredNodeState) (*agentv1.DesiredNodeState, map[string]pullCredential) {
	clean := proto.Clone(state).(*agentv1.DesiredNodeState)
	credentials := make(map[string]pullCredential)
	for _, service := range clean.GetServices() {
		if service.GetRegistryUsername() != "" || service.GetRegistryPassword() != "" {
			credentials[service.GetAllocationId()] = pullCredential{Username: service.GetRegistryUsername(), Password: service.GetRegistryPassword()}
		}
		service.RegistryUsername = ""
		service.RegistryPassword = ""
	}
	return clean, credentials
}

func desiredConfigurationEqual(a, b *agentv1.DesiredNodeState) bool {
	if a == nil || b == nil {
		return a == b
	}
	left := proto.Clone(a).(*agentv1.DesiredNodeState)
	right := proto.Clone(b).(*agentv1.DesiredNodeState)
	left.GeneratedAt, right.GeneratedAt = nil, nil
	return proto.Equal(left, right)
}

func statusConfigurationEqual(a, b *agentv1.StatusReport) bool {
	if a == nil || b == nil {
		return a == b
	}
	left := proto.Clone(a).(*agentv1.StatusReport)
	right := proto.Clone(b).(*agentv1.StatusReport)
	left.SessionId, right.SessionId = "", ""
	left.ObservationSequence, right.ObservationSequence = 0, 0
	return proto.Equal(left, right)
}

func replaceCredentials(bucket *bbolt.Bucket, credentials map[string]pullCredential) error {
	var keys [][]byte
	if err := bucket.ForEach(func(key, _ []byte) error {
		keys = append(keys, bytes.Clone(key))
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	for allocationID, credential := range credentials {
		raw, err := json.Marshal(credential)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(allocationID), raw); err != nil {
			return err
		}
	}
	return nil
}

func updateDesiredAllocations(bucket *bbolt.Bucket, state *agentv1.DesiredNodeState) error {
	desired := make(map[string]*agentv1.DesiredService, len(state.GetServices()))
	for _, service := range state.GetServices() {
		desired[service.GetAllocationId()] = service
	}
	var existing []string
	if err := bucket.ForEach(func(key, _ []byte) error {
		existing = append(existing, string(key))
		return nil
	}); err != nil {
		return err
	}
	for _, allocationID := range existing {
		if _, ok := desired[allocationID]; ok {
			continue
		}
		allocation, err := readAllocation(bucket, allocationID)
		if err != nil {
			return err
		}
		if allocation.PendingOperation != "establish-ownership" {
			if allocation.Terminal && allocation.RuntimeID == "" {
				continue
			}
			allocation.PendingOperation = "stop"
		}
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	for _, service := range state.GetServices() {
		allocation, err := readAllocation(bucket, service.GetAllocationId())
		if err != nil {
			return err
		}
		allocation.AllocationID = service.GetAllocationId()
		allocation.DesiredSpecRevision = service.GetDesiredSpecRevision()
		allocation.DesiredGeneration = service.GetDesiredRolloutGeneration()
		allocation.Terminal = false
		if service.GetIntent() == agentv1.AllocationIntent_ALLOCATION_INTENT_DRAIN {
			allocation.PendingOperation = "drain"
			if service.GetDrainDeadline() != nil {
				allocation.DrainDeadline = service.GetDrainDeadline().AsTime().UTC().Format(time.RFC3339Nano)
			}
		} else if allocation.AppliedGeneration < allocation.DesiredGeneration || allocation.AppliedSpecRevision < allocation.DesiredSpecRevision {
			allocation.PendingOperation = "start-or-update"
			allocation.DrainDeadline = ""
		} else {
			allocation.PendingOperation = ""
			allocation.DrainDeadline = ""
		}
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	return nil
}

func recoveryOwnershipEstablished(bucket *bbolt.Bucket, state *agentv1.DesiredNodeState) (bool, error) {
	desiredAllocations := make(map[string]struct{}, len(state.GetServices()))
	for _, service := range state.GetServices() {
		desiredAllocations[service.GetAllocationId()] = struct{}{}
	}
	desiredVolumes := make(map[string]struct{}, len(state.GetVolumes()))
	for _, volume := range state.GetVolumes() {
		desiredVolumes[volume.GetVolumeId()] = struct{}{}
	}
	established := true
	err := bucket.ForEach(func(_, value []byte) error {
		var resource RuntimeResource
		if err := json.Unmarshal(value, &resource); err != nil {
			return err
		}
		if resource.AllocationID != "" {
			if _, ok := desiredAllocations[resource.AllocationID]; !ok {
				established = false
			}
		} else if resource.VolumeID != "" {
			if _, ok := desiredVolumes[resource.VolumeID]; !ok {
				established = false
			}
		}
		return nil
	})
	return established, err
}

func runtimeResourceKey(resource RuntimeResource) ([]byte, error) {
	switch {
	case resource.AllocationID != "" && resource.VolumeID == "":
		if err := validateRuntimeID("allocation ID", resource.AllocationID); err != nil {
			return nil, err
		}
		return []byte("allocation:" + resource.AllocationID), nil
	case resource.VolumeID != "" && resource.AllocationID == "":
		if err := validateRuntimeID("volume ID", resource.VolumeID); err != nil {
			return nil, err
		}
		return []byte("volume:" + resource.VolumeID), nil
	default:
		return nil, errors.New("runtime resource must identify exactly one allocation or volume")
	}
}

func clearBucket(bucket *bbolt.Bucket) error {
	var keys [][]byte
	if err := bucket.ForEach(func(key, _ []byte) error {
		keys = append(keys, bytes.Clone(key))
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func writeRuntimeInventory(bucket *bbolt.Bucket, inventory []RuntimeResource) error {
	if err := clearBucket(bucket); err != nil {
		return err
	}
	for _, resource := range inventory {
		key, err := runtimeResourceKey(resource)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(resource)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, raw); err != nil {
			return err
		}
	}
	return nil
}

func applyReportToAllocations(bucket *bbolt.Bucket, report *agentv1.StatusReport) error {
	seen := make(map[string]struct{}, len(report.GetServices()))
	for _, condition := range report.GetServices() {
		seen[condition.GetAllocationId()] = struct{}{}
		allocation, err := readAllocation(bucket, condition.GetAllocationId())
		if err != nil {
			return err
		}
		allocation.AllocationID = condition.GetAllocationId()
		allocation.AppliedSpecRevision = condition.GetAppliedSpecRevision()
		allocation.AppliedGeneration = condition.GetAppliedRolloutGeneration()
		allocation.Phase = condition.GetPhase()
		allocation.Terminal = condition.GetPhase() == "Drained"
		switch allocation.PendingOperation {
		case "drain":
			if allocation.Terminal {
				allocation.PendingOperation = ""
			}
		case "start-or-update":
			if condition.GetPhase() != "Error" && allocation.AppliedSpecRevision >= allocation.DesiredSpecRevision && allocation.AppliedGeneration >= allocation.DesiredGeneration {
				allocation.PendingOperation = ""
			}
		}
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	var completed []localAllocationState
	if err := bucket.ForEach(func(key, value []byte) error {
		if _, ok := seen[string(key)]; ok {
			return nil
		}
		var allocation localAllocationState
		if err := json.Unmarshal(value, &allocation); err != nil {
			return err
		}
		if allocation.PendingOperation == "stop" {
			allocation.PendingOperation = ""
			allocation.RuntimeID = ""
			allocation.Terminal = true
			allocation.Phase = "Stopped"
			completed = append(completed, allocation)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, allocation := range completed {
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	return nil
}

func readAllocation(bucket *bbolt.Bucket, allocationID string) (localAllocationState, error) {
	raw := bucket.Get([]byte(allocationID))
	if len(raw) == 0 {
		return localAllocationState{AllocationID: allocationID}, nil
	}
	var allocation localAllocationState
	if err := json.Unmarshal(raw, &allocation); err != nil {
		return localAllocationState{}, fmt.Errorf("decode allocation %s: %w", allocationID, err)
	}
	return allocation, nil
}

func writeAllocation(bucket *bbolt.Bucket, allocation localAllocationState) error {
	raw, err := json.Marshal(allocation)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(allocation.AllocationID), raw)
}

func clearAllocationRuntimeIdentities(bucket *bbolt.Bucket) error {
	var allocations []localAllocationState
	if err := bucket.ForEach(func(_, value []byte) error {
		var allocation localAllocationState
		if err := json.Unmarshal(value, &allocation); err != nil {
			return err
		}
		if allocation.RuntimeID != "" {
			allocation.RuntimeID = ""
			allocations = append(allocations, allocation)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, allocation := range allocations {
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	return nil
}

func validateIntegerEncoding(bucket *bbolt.Bucket, key []byte) error {
	if raw := bucket.Get(key); len(raw) != 0 && len(raw) != 8 {
		return fmt.Errorf("invalid integer encoding for %s", key)
	}
	return nil
}

func validateClusterIdentity(meta *bbolt.Bucket, clusterID string) error {
	existing := string(meta.Get(clusterIdentityKey))
	if existing != "" && clusterID != "" && existing != clusterID {
		return fmt.Errorf("local state belongs to cluster %q, authenticated cluster is %q", existing, clusterID)
	}
	return nil
}

func putUint64(bucket *bbolt.Bucket, key []byte, value uint64) error {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	return bucket.Put(key, raw[:])
}

func readUint64(raw []byte) uint64 {
	if len(raw) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw)
}

func putInt64(bucket *bbolt.Bucket, key []byte, value int64) error {
	return putUint64(bucket, key, uint64(value))
}

func readInt64(raw []byte) int64 {
	return int64(readUint64(raw))
}
