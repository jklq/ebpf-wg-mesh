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
	"ebof-wg-mesh/internal/reconciliation"

	"github.com/google/uuid"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

const (
	localStateFormatVersion uint64 = 3
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
	identityRecoveryKey    = []byte("identity_recovery")
	sessionIncarnationKey  = []byte("session_incarnation")
	localStoreIDKey        = []byte("local_store_id")
	initStateKey           = []byte("initialization_state")
	agentIdentityKey       = []byte("agent_identity")
	clusterIdentityKey     = []byte("cluster_identity")
	highestEpochKey        = []byte("highest_observed_epoch")
	authorityEpochKey      = []byte("accepted_authority_epoch")
	cursorKey              = []byte("reconciliation_cursor")
	desiredStateKey        = []byte("accepted_state")
	stagedDesiredStateKey  = []byte("staged_state")
	stagedDiffKey          = []byte("staged_diff")
	stagedNodeConfigKey    = []byte("staged_node_config")
	nodeConfigVersionKey   = []byte("node_config_version")
	credentialsVersionKey  = []byte("credentials_version")
	replicasVersionKey     = []byte("replicas_version")
	statusReportKey        = []byte("status_report")
	reportEpochKey         = []byte("report_authority_epoch")
	reportCursorKey        = []byte("report_reconciliation_cursor")
	observationSequenceKey = []byte("observation_sequence")
	replicaDiscoveryKey    = []byte("replica_discovery")
)

type initializationState string

const (
	initializationUninitialized initializationState = "uninitialized"
	initializationReady         initializationState = "ready"
	initializationRecovery      initializationState = "recovery"
)

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
	Phase               string `json:"phase,omitempty"`
}

type pullCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type replicaDiscoveryState struct {
	Seeds    []string `json:"seeds"`
	Replicas []string `json:"replicas"`
}

type localStateSummary struct {
	LocalStoreID              string
	Allocations               []*agentv1.ServiceCondition
	Initialization            initializationState
	AgentIdentity             string
	ClusterIdentity           string
	AuthorityEpoch            uint64
	ReconciliationCursor      int64
	NodeConfigVersion         string
	CredentialsVersion        string
	ReplicasVersion           string
	ObservationOverlayVersion string
	RuntimeResources          []RuntimeResource
	QuarantinedStore          string
}

type localStateStore struct {
	path        string
	db          *bbolt.DB
	quarantined string
	now         func() time.Time
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
	store := &localStateStore{path: path, db: db, now: time.Now}
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
	store := &localStateStore{path: path, db: recreated, quarantined: quarantined, now: time.Now}
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
		// Staged candidates have no acceptance decision. A crash must never
		// promote them, even if the grant was valid when persistence started.
		for _, key := range [][]byte{stagedDesiredStateKey, stagedDiffKey, stagedNodeConfigKey} {
			if err := tx.Bucket(localDesiredBucket).Delete(key); err != nil {
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
		if len(meta.Get(localStoreIDKey)) == 0 {
			if err := meta.Put(localStoreIDKey, []byte(uuid.NewString())); err != nil {
				return err
			}
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
		for _, key := range [][]byte{authorityEpochKey, highestEpochKey, cursorKey, sessionIncarnationKey} {
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
			if desired.GetNodeConfigVersion() != string(meta.Get(nodeConfigVersionKey)) {
				return errors.New("accepted node config version does not match local version")
			}
		} else if readUint64(meta.Get(authorityEpochKey)) != 0 || readInt64(meta.Get(cursorKey)) != 0 {
			return errors.New("local reconciliation position exists without desired state")
		}
		// Credentials are independently versioned and may arrive before their
		// checkpoint; they are not validated against current desired here.
		if err := tx.Bucket(localCredentialsBucket).ForEach(func(key, value []byte) error {
			if err := validateRuntimeID("allocation ID", string(key)); err != nil {
				return err
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
		if raw := meta.Get(replicaDiscoveryKey); len(raw) > 0 {
			var discovery replicaDiscoveryState
			if err := json.Unmarshal(raw, &discovery); err != nil {
				return fmt.Errorf("decode replica discovery state: %w", err)
			}
			normalized := normalizeReplicaDiscoveryState(discovery)
			if !stringSlicesEqual(discovery.Seeds, normalized.Seeds) || !stringSlicesEqual(discovery.Replicas, normalized.Replicas) {
				return errors.New("replica discovery state contains blank or duplicate addresses")
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

func (s *localStateStore) setReplicaSeeds(addresses []string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		discovery, err := readReplicaDiscovery(tx.Bucket(localMetaBucket))
		if err != nil {
			return err
		}
		discovery.Seeds = normalizeAddresses(addresses)
		return writeReplicaDiscovery(tx.Bucket(localMetaBucket), discovery)
	})
}

func (s *localStateStore) setReplicaAddresses(addresses []string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		discovery, err := readReplicaDiscovery(tx.Bucket(localMetaBucket))
		if err != nil {
			return err
		}
		discovery.Replicas = normalizeAddresses(addresses)
		return writeReplicaDiscovery(tx.Bucket(localMetaBucket), discovery)
	})
}

func (s *localStateStore) replicaAddresses() ([]string, error) {
	var addresses []string
	err := s.db.View(func(tx *bbolt.Tx) error {
		discovery, err := readReplicaDiscovery(tx.Bucket(localMetaBucket))
		if err != nil {
			return err
		}
		addresses = appendUniqueAddresses(nil, discovery.Seeds...)
		addresses = appendUniqueAddresses(addresses, discovery.Replicas...)
		return nil
	})
	return addresses, err
}

func readReplicaDiscovery(meta *bbolt.Bucket) (replicaDiscoveryState, error) {
	var discovery replicaDiscoveryState
	if raw := meta.Get(replicaDiscoveryKey); len(raw) > 0 {
		if err := json.Unmarshal(raw, &discovery); err != nil {
			return replicaDiscoveryState{}, fmt.Errorf("decode replica discovery state: %w", err)
		}
	}
	return normalizeReplicaDiscoveryState(discovery), nil
}

func writeReplicaDiscovery(meta *bbolt.Bucket, discovery replicaDiscoveryState) error {
	raw, err := json.Marshal(normalizeReplicaDiscoveryState(discovery))
	if err != nil {
		return fmt.Errorf("encode replica discovery state: %w", err)
	}
	return meta.Put(replicaDiscoveryKey, raw)
}

func normalizeReplicaDiscoveryState(discovery replicaDiscoveryState) replicaDiscoveryState {
	return replicaDiscoveryState{
		Seeds:    normalizeAddresses(discovery.Seeds),
		Replicas: normalizeAddresses(discovery.Replicas),
	}
}

func normalizeAddresses(addresses []string) []string {
	if len(addresses) == 0 {
		return nil
	}
	result := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result
}

func appendUniqueAddresses(addresses []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(addresses)+len(additions))
	result := make([]string, 0, len(addresses)+len(additions))
	for _, address := range append(append([]string(nil), addresses...), additions...) {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		result = append(result, address)
	}
	return result
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (s *localStateStore) prepareStartup(clusterID string, inventory []RuntimeResource) error {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return err
	}
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

func (s *localStateStore) acceptDesired(clusterID, sessionID string, incoming *agentv1.DesiredNodeState) (bool, error) {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return false, err
	}
	if incoming != nil {
		if err := s.db.View(func(tx *bbolt.Tx) error {
			if incoming.GetAgentId() != string(tx.Bucket(localMetaBucket).Get(agentIdentityKey)) {
				return errors.New("snapshot agent identity does not match local identity")
			}
			return nil
		}); err != nil {
			return false, err
		}
		if incoming.GetClusterId() != clusterID {
			return false, errors.New("snapshot cluster identity does not match authenticated cluster")
		}
		if sessionID == "" || incoming.GetSessionId() != sessionID {
			return false, errors.New("desired state belongs to another session")
		}
		if err := s.observeAuthorityEpoch(incoming.GetAuthorityEpoch()); err != nil {
			return false, err
		}
	}
	if incoming == nil {
		return false, errors.New("desired state is nil")
	}
	if err := validateDesiredState(incoming); err != nil {
		return false, err
	}
	staged, err := s.stageDesired(sessionID, incoming)
	if err != nil {
		return false, err
	}
	return s.acceptStagedDesired(clusterID, sessionID, staged)
}

func (s *localStateStore) stageDesired(sessionID string, incoming *agentv1.DesiredNodeState) ([]byte, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(incoming)
	if err != nil {
		return nil, fmt.Errorf("encode desired candidate: %w", err)
	}
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if err := reconciliation.ValidateCommand(incoming, sessionID, s.now()); err != nil {
			return err
		}
		return tx.Bucket(localDesiredBucket).Put(stagedDesiredStateKey, encoded)
	})
	return encoded, err
}

func (s *localStateStore) acceptStagedDesired(clusterID, sessionID string, staged []byte) (bool, error) {
	incoming := &agentv1.DesiredNodeState{}
	if err := proto.Unmarshal(staged, incoming); err != nil {
		return false, fmt.Errorf("decode desired candidate: %w", err)
	}
	clean, _ := splitDesiredCredentials(incoming)
	canonicalizeDesiredState(clean)
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(clean)
	if err != nil {
		return false, fmt.Errorf("encode desired state: %w", err)
	}
	changed := false
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if !bytes.Equal(tx.Bucket(localDesiredBucket).Get(stagedDesiredStateKey), staged) {
			return errors.New("desired candidate is not staged")
		}
		meta := tx.Bucket(localMetaBucket)
		if incoming.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return fmt.Errorf("desired state agent %q does not match local identity %q", incoming.GetAgentId(), meta.Get(agentIdentityKey))
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
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
		case incoming.GetAuthorityEpoch() < max(acceptedEpoch, readUint64(meta.Get(highestEpochKey))):
			return fmt.Errorf("stale authority epoch %d", incoming.GetAuthorityEpoch())
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
			// Node configuration is independently versioned: a same-cursor
			// repair checkpoint carries the latest node configuration and
			// applies it below. The observation overlay (internal hosts,
			// restart observations) derives from live control-plane state the
			// same way and is repaired by the same checkpoint.
			changed = previousState.GetNodeConfigVersion() != clean.GetNodeConfigVersion() ||
				reconciliation.HashObservationOverlay(previousState.GetServices()) != reconciliation.HashObservationOverlay(clean.GetServices())
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
		// 2.10: checkpoints carry node config but never credentials;
		// credentials arrive in PullCredentialSet and are left untouched.
		if err := meta.Put(nodeConfigVersionKey, []byte(incoming.GetNodeConfigVersion())); err != nil {
			return err
		}
		if err := updateDesiredAllocations(tx.Bucket(localAllocationsBucket), clean); err != nil {
			return err
		}
		if initializationState(meta.Get(initStateKey)) == initializationUninitialized {
			if err := meta.Put(initStateKey, []byte(initializationReady)); err != nil {
				return err
			}
		}
		if initializationState(meta.Get(initStateKey)) == initializationRecovery {
			established, err := recoveryOwnershipEstablished(tx.Bucket(localInventoryBucket), clean)
			if err != nil {
				return err
			}
			if established {
				if err := meta.Put(initStateKey, []byte(initializationReady)); err != nil {
					return err
				}
			}
		}
		if err := desired.Delete(stagedDesiredStateKey); err != nil {
			return err
		}
		return reconciliation.ValidateCommand(incoming, sessionID, s.now())
	})
	return changed && err == nil, err
}

func (s *localStateStore) acceptAllocationDiff(clusterID, sessionID string, diff *agentv1.AllocationDiff) (bool, error) {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return false, err
	}
	if diff != nil {
		if err := s.db.View(func(tx *bbolt.Tx) error {
			if diff.GetAgentId() != string(tx.Bucket(localMetaBucket).Get(agentIdentityKey)) {
				return errors.New("diff agent identity does not match local identity")
			}
			return nil
		}); err != nil {
			return false, err
		}
		if diff.GetClusterId() != clusterID {
			return false, errors.New("diff cluster identity does not match authenticated cluster")
		}
		if sessionID == "" || diff.GetSessionId() != sessionID {
			return false, errors.New("allocation diff belongs to another session")
		}
		if err := s.observeAuthorityEpoch(diff.GetAuthorityEpoch()); err != nil {
			return false, err
		}
	}
	if err := validateAllocationDiff(diff); err != nil {
		return false, err
	}
	staged, err := s.stageDiff(sessionID, diff)
	if err != nil {
		return false, err
	}
	return s.acceptStagedDiff(clusterID, sessionID, staged)
}

func (s *localStateStore) stageDiff(sessionID string, diff *agentv1.AllocationDiff) ([]byte, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(diff)
	if err != nil {
		return nil, fmt.Errorf("encode diff candidate: %w", err)
	}
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if err := reconciliation.ValidateCommand(diff, sessionID, s.now()); err != nil {
			return err
		}
		return tx.Bucket(localDesiredBucket).Put(stagedDiffKey, encoded)
	})
	return encoded, err
}

func (s *localStateStore) acceptStagedDiff(clusterID, sessionID string, staged []byte) (bool, error) {
	diff := &agentv1.AllocationDiff{}
	if err := proto.Unmarshal(staged, diff); err != nil {
		return false, fmt.Errorf("decode diff candidate: %w", err)
	}
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		if !bytes.Equal(tx.Bucket(localDesiredBucket).Get(stagedDiffKey), staged) {
			return errors.New("diff candidate is not staged")
		}
		meta := tx.Bucket(localMetaBucket)
		if diff.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return fmt.Errorf("diff agent %q does not match local identity %q", diff.GetAgentId(), meta.Get(agentIdentityKey))
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
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
		if diff.GetAuthorityEpoch() < max(acceptedEpoch, readUint64(meta.Get(highestEpochKey))) {
			return fmt.Errorf("stale authority epoch %d", diff.GetAuthorityEpoch())
		}
		if diff.GetTargetRevision() <= acceptedCursor {
			if diff.GetTargetRevision() == acceptedCursor {
				// Duplicate of already-applied diff; idempotent, no re-apply.
				if err := tx.Bucket(localDesiredBucket).Delete(stagedDiffKey); err != nil {
					return err
				}
				return reconciliation.ValidateCommand(diff, sessionID, s.now())
			}
			return fmt.Errorf("stale diff target %d follows %d", diff.GetTargetRevision(), acceptedCursor)
		}
		if diff.GetBaseRevision() != acceptedCursor {
			return fmt.Errorf("diff base %d does not match accepted cursor %d; checkpoint required", diff.GetBaseRevision(), acceptedCursor)
		}
		desired := tx.Bucket(localDesiredBucket)
		previousRaw := desired.Get(desiredStateKey)
		if len(previousRaw) == 0 {
			return errors.New("allocation diff requires an accepted checkpoint first")
		}
		var previous agentv1.DesiredNodeState
		if err := proto.Unmarshal(previousRaw, &previous); err != nil {
			return fmt.Errorf("decode accepted desired state: %w", err)
		}
		merged, err := applyDiffToState(&previous, diff)
		if err != nil {
			return err
		}
		if err := validateDesiredState(merged); err != nil {
			return fmt.Errorf("merged diff state: %w", err)
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(merged)
		if err != nil {
			return fmt.Errorf("encode merged desired state: %w", err)
		}
		if err := desired.Put(desiredStateKey, encoded); err != nil {
			return err
		}
		if err := putUint64(meta, authorityEpochKey, diff.GetAuthorityEpoch()); err != nil {
			return err
		}
		if err := putInt64(meta, cursorKey, diff.GetTargetRevision()); err != nil {
			return err
		}
		if err := updateDesiredAllocations(tx.Bucket(localAllocationsBucket), merged); err != nil {
			return err
		}
		if initializationState(meta.Get(initStateKey)) == initializationUninitialized {
			if err := meta.Put(initStateKey, []byte(initializationReady)); err != nil {
				return err
			}
		}
		if initializationState(meta.Get(initStateKey)) == initializationRecovery {
			established, err := recoveryOwnershipEstablished(tx.Bucket(localInventoryBucket), merged)
			if err != nil {
				return err
			}
			if established {
				if err := meta.Put(initStateKey, []byte(initializationReady)); err != nil {
					return err
				}
			}
		}
		changed = true
		if err := desired.Delete(stagedDiffKey); err != nil {
			return err
		}
		return reconciliation.ValidateCommand(diff, sessionID, s.now())
	})
	return changed && err == nil, err
}

// applyDiffToState merges starts/updates/stops into previous. Starts and
// updates upsert; stops delete idempotently. Node config and version are
// preserved; only the allocation cursor/epoch advance.
func applyDiffToState(previous *agentv1.DesiredNodeState, diff *agentv1.AllocationDiff) (*agentv1.DesiredNodeState, error) {
	if previous == nil {
		return nil, errors.New("previous desired state is nil")
	}
	merged := proto.Clone(previous).(*agentv1.DesiredNodeState)
	merged.AuthorityEpoch = diff.GetAuthorityEpoch()
	merged.ReconciliationCursor = diff.GetTargetRevision()
	merged.GeneratedAt = diff.GetGeneratedAt()
	merged.SessionId = diff.GetSessionId()
	merged.AuthorityNotAfter = diff.GetAuthorityNotAfter()
	services := make(map[string]*agentv1.DesiredService, len(merged.GetServices()))
	for _, svc := range merged.GetServices() {
		services[svc.GetAllocationId()] = svc
	}
	for _, id := range diff.GetStops() {
		delete(services, id)
	}
	for _, svc := range diff.GetStarts() {
		services[svc.GetAllocationId()] = proto.Clone(svc).(*agentv1.DesiredService)
	}
	for _, svc := range diff.GetUpdates() {
		services[svc.GetAllocationId()] = proto.Clone(svc).(*agentv1.DesiredService)
	}
	merged.Services = merged.Services[:0]
	for _, svc := range services {
		merged.Services = append(merged.Services, svc)
	}
	volumes := make(map[string]*agentv1.DesiredVolume, len(merged.GetVolumes()))
	for _, v := range merged.GetVolumes() {
		volumes[v.GetVolumeId()] = v
	}
	for _, id := range diff.GetVolumeStops() {
		delete(volumes, id)
	}
	for _, v := range diff.GetVolumeStarts() {
		volumes[v.GetVolumeId()] = proto.Clone(v).(*agentv1.DesiredVolume)
	}
	merged.Volumes = merged.Volumes[:0]
	for _, v := range volumes {
		merged.Volumes = append(merged.Volumes, v)
	}
	canonicalizeDesiredState(merged)
	return merged, nil
}

func (s *localStateStore) acceptNodeConfigUpdate(clusterID, sessionID string, update *agentv1.NodeConfigUpdate) (bool, error) {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return false, err
	}
	if update == nil {
		return false, errors.New("node config update is nil")
	}
	if err := s.db.View(func(tx *bbolt.Tx) error {
		if update.GetAgentId() != string(tx.Bucket(localMetaBucket).Get(agentIdentityKey)) {
			return errors.New("node config agent identity does not match local identity")
		}
		return nil
	}); err != nil {
		return false, err
	}
	if update.GetClusterId() != clusterID {
		return false, errors.New("node config cluster identity does not match authenticated cluster")
	}
	if sessionID == "" || update.GetSessionId() != sessionID {
		return false, errors.New("node config belongs to another session")
	}
	if err := s.observeAuthorityEpoch(update.GetAuthorityEpoch()); err != nil {
		return false, err
	}
	if update.GetNodeConfig() == nil || strings.TrimSpace(update.GetNodeConfigVersion()) == "" {
		return false, errors.New("node config update requires config and version")
	}
	if want := reconciliation.HashNodeConfig(update.GetNodeConfig()); want != update.GetNodeConfigVersion() {
		return false, errors.New("node config version does not match config")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(update)
	if err != nil {
		return false, fmt.Errorf("encode node config candidate: %w", err)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := reconciliation.ValidateCommand(update, sessionID, s.now()); err != nil {
			return err
		}
		return tx.Bucket(localDesiredBucket).Put(stagedNodeConfigKey, encoded)
	}); err != nil {
		return false, err
	}
	changed := false
	err = s.db.Update(func(tx *bbolt.Tx) error {
		if !bytes.Equal(tx.Bucket(localDesiredBucket).Get(stagedNodeConfigKey), encoded) {
			return errors.New("node config candidate is not staged")
		}
		meta := tx.Bucket(localMetaBucket)
		if update.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return errors.New("node config agent identity does not match local identity")
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
		if string(meta.Get(clusterIdentityKey)) == "" {
			if clusterID == "" {
				return errors.New("authenticated cluster identity is unavailable")
			}
			if err := meta.Put(clusterIdentityKey, []byte(clusterID)); err != nil {
				return err
			}
		}
		if update.GetAuthorityEpoch() < max(readUint64(meta.Get(authorityEpochKey)), readUint64(meta.Get(highestEpochKey))) {
			return fmt.Errorf("stale authority epoch %d", update.GetAuthorityEpoch())
		}
		if string(meta.Get(nodeConfigVersionKey)) == update.GetNodeConfigVersion() {
			if err := tx.Bucket(localDesiredBucket).Delete(stagedNodeConfigKey); err != nil {
				return err
			}
			return reconciliation.ValidateCommand(update, sessionID, s.now())
		}
		desired := tx.Bucket(localDesiredBucket)
		previousRaw := desired.Get(desiredStateKey)
		if len(previousRaw) == 0 {
			return errors.New("node config update requires an accepted checkpoint first")
		}
		var previous agentv1.DesiredNodeState
		if err := proto.Unmarshal(previousRaw, &previous); err != nil {
			return fmt.Errorf("decode accepted desired state: %w", err)
		}
		previous.NodeConfig = proto.Clone(update.GetNodeConfig()).(*agentv1.AssignedNodeConfig)
		previous.NodeConfigVersion = update.GetNodeConfigVersion()
		if err := validateDesiredState(&previous); err != nil {
			return fmt.Errorf("merged node config state: %w", err)
		}
		merged, err := proto.MarshalOptions{Deterministic: true}.Marshal(&previous)
		if err != nil {
			return err
		}
		if err := desired.Put(desiredStateKey, merged); err != nil {
			return err
		}
		if err := meta.Put(nodeConfigVersionKey, []byte(update.GetNodeConfigVersion())); err != nil {
			return err
		}
		changed = true
		if err := desired.Delete(stagedNodeConfigKey); err != nil {
			return err
		}
		return reconciliation.ValidateCommand(update, sessionID, s.now())
	})
	return changed && err == nil, err
}

func (s *localStateStore) acceptPullCredentials(clusterID, sessionID string, creds *agentv1.PullCredentialSet) (bool, error) {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return false, err
	}
	if creds == nil {
		return false, errors.New("pull credentials are nil")
	}
	if strings.TrimSpace(creds.GetAgentId()) == "" {
		return false, errors.New("pull credentials agent_id is required")
	}
	if creds.GetClusterId() != clusterID {
		return false, errors.New("pull credentials cluster identity does not match authenticated cluster")
	}
	if sessionID == "" || creds.GetSessionId() != sessionID {
		return false, errors.New("pull credentials belong to another session")
	}
	if err := s.observeAuthorityEpoch(creds.GetAuthorityEpoch()); err != nil {
		return false, err
	}
	if strings.TrimSpace(creds.GetCredentialsVersion()) == "" {
		return false, errors.New("pull credentials version is required")
	}
	if want := reconciliation.HashCredentials(creds.GetCredentials()); want != creds.GetCredentialsVersion() {
		return false, errors.New("pull credentials version does not match credentials")
	}
	for _, c := range creds.GetCredentials() {
		if err := validateRuntimeID("allocation ID", c.GetAllocationId()); err != nil {
			return false, err
		}
	}
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if creds.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return errors.New("pull credentials agent identity does not match local identity")
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
		if string(meta.Get(clusterIdentityKey)) == "" && clusterID != "" {
			if err := meta.Put(clusterIdentityKey, []byte(clusterID)); err != nil {
				return err
			}
		}
		if creds.GetAuthorityEpoch() < max(readUint64(meta.Get(authorityEpochKey)), readUint64(meta.Get(highestEpochKey))) {
			return fmt.Errorf("stale authority epoch %d", creds.GetAuthorityEpoch())
		}
		if string(meta.Get(credentialsVersionKey)) == creds.GetCredentialsVersion() {
			return reconciliation.ValidateCommand(creds, sessionID, s.now())
		}
		mapped := make(map[string]pullCredential, len(creds.GetCredentials()))
		for _, c := range creds.GetCredentials() {
			mapped[c.GetAllocationId()] = pullCredential{Username: c.GetUsername(), Password: c.GetPassword()}
		}
		if err := replaceCredentials(tx.Bucket(localCredentialsBucket), mapped); err != nil {
			return err
		}
		if err := meta.Put(credentialsVersionKey, []byte(creds.GetCredentialsVersion())); err != nil {
			return err
		}
		changed = true
		return reconciliation.ValidateCommand(creds, sessionID, s.now())
	})
	return changed && err == nil, err
}

func (s *localStateStore) acceptReplicaEndpoints(clusterID, sessionID string, replicas *agentv1.ReplicaEndpoints) (bool, error) {
	if err := s.requireClusterIdentity(clusterID); err != nil {
		return false, err
	}
	if replicas == nil {
		return false, errors.New("replica endpoints are nil")
	}
	if strings.TrimSpace(replicas.GetAgentId()) == "" {
		return false, errors.New("replica endpoints agent_id is required")
	}
	if replicas.GetClusterId() != clusterID {
		return false, errors.New("replica endpoints cluster identity does not match authenticated cluster")
	}
	if sessionID == "" || replicas.GetSessionId() != sessionID {
		return false, errors.New("replica endpoints belong to another session")
	}
	if err := s.observeAuthorityEpoch(replicas.GetAuthorityEpoch()); err != nil {
		return false, err
	}
	if strings.TrimSpace(replicas.GetReplicasVersion()) == "" {
		return false, errors.New("replica endpoints version is required")
	}
	if want := reconciliation.HashReplicas(replicas.GetReplicaAddresses()); want != replicas.GetReplicasVersion() {
		return false, errors.New("replica endpoints version does not match addresses")
	}
	changed := false
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if replicas.GetAgentId() != string(meta.Get(agentIdentityKey)) {
			return errors.New("replica endpoints agent identity does not match local identity")
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			return err
		}
		if string(meta.Get(clusterIdentityKey)) == "" && clusterID != "" {
			if err := meta.Put(clusterIdentityKey, []byte(clusterID)); err != nil {
				return err
			}
		}
		if replicas.GetAuthorityEpoch() < max(readUint64(meta.Get(authorityEpochKey)), readUint64(meta.Get(highestEpochKey))) {
			return fmt.Errorf("stale authority epoch %d", replicas.GetAuthorityEpoch())
		}
		if string(meta.Get(replicasVersionKey)) == replicas.GetReplicasVersion() {
			return reconciliation.ValidateCommand(replicas, sessionID, s.now())
		}
		discovery, err := readReplicaDiscovery(meta)
		if err != nil {
			return err
		}
		discovery.Replicas = normalizeAddresses(replicas.GetReplicaAddresses())
		if err := writeReplicaDiscovery(meta, discovery); err != nil {
			return err
		}
		if err := meta.Put(replicasVersionKey, []byte(replicas.GetReplicasVersion())); err != nil {
			return err
		}
		changed = true
		return reconciliation.ValidateCommand(replicas, sessionID, s.now())
	})
	return changed && err == nil, err
}

func validateDesiredState(state *agentv1.DesiredNodeState) error {
	if state.GetClusterId() == "" {
		return errors.New("desired state cluster_id is required")
	}
	if state.GetScope() != agentv1.SnapshotScope_SNAPSHOT_SCOPE_AGENT || !state.GetComplete() {
		return errors.New("desired state requires a complete authoritative agent scope")
	}
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
	if strings.TrimSpace(state.GetNodeConfigVersion()) == "" {
		return errors.New("desired state node_config_version is required")
	}
	if want := reconciliation.HashNodeConfig(state.GetNodeConfig()); want != state.GetNodeConfigVersion() {
		return errors.New("desired state node_config_version does not match node config")
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
		if service.GetRegistryUsername() != "" || service.GetRegistryPassword() != "" {
			return fmt.Errorf("desired allocation %q must not carry registry credentials on the wire", allocationID)
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

func validateAllocationDiff(diff *agentv1.AllocationDiff) error {
	if diff == nil {
		return errors.New("allocation diff is nil")
	}
	if strings.TrimSpace(diff.GetAgentId()) == "" {
		return errors.New("allocation diff agent_id is required")
	}
	if diff.GetClusterId() == "" {
		return errors.New("allocation diff cluster_id is required")
	}
	if diff.GetAuthorityEpoch() == 0 {
		return errors.New("allocation diff authority_epoch must be greater than zero")
	}
	if diff.GetTargetRevision() <= diff.GetBaseRevision() || diff.GetBaseRevision() < 0 {
		return errors.New("allocation diff revisions must advance")
	}
	seen := make(map[string]struct{})
	for _, svc := range diff.GetStarts() {
		id := svc.GetAllocationId()
		if err := validateRuntimeID("allocation ID", id); err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("duplicate diff allocation %q", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(svc.GetServiceId()) == "" || svc.GetDesiredSpecRevision() <= 0 || svc.GetDesiredRolloutGeneration() <= 0 {
			return fmt.Errorf("diff start %q has incomplete identity or generation", id)
		}
		if svc.GetRegistryUsername() != "" || svc.GetRegistryPassword() != "" {
			return fmt.Errorf("diff start %q must not carry registry credentials", id)
		}
	}
	for _, svc := range diff.GetUpdates() {
		id := svc.GetAllocationId()
		if err := validateRuntimeID("allocation ID", id); err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("duplicate diff allocation %q", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(svc.GetServiceId()) == "" || svc.GetDesiredSpecRevision() <= 0 || svc.GetDesiredRolloutGeneration() <= 0 {
			return fmt.Errorf("diff update %q has incomplete identity or generation", id)
		}
		if svc.GetRegistryUsername() != "" || svc.GetRegistryPassword() != "" {
			return fmt.Errorf("diff update %q must not carry registry credentials", id)
		}
	}
	stops := make(map[string]struct{}, len(diff.GetStops()))
	for _, id := range diff.GetStops() {
		if err := validateRuntimeID("allocation ID", id); err != nil {
			return err
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("duplicate diff allocation %q", id)
		}
		if _, dup := stops[id]; dup {
			return fmt.Errorf("duplicate diff stop %q", id)
		}
		seen[id] = struct{}{}
		stops[id] = struct{}{}
	}
	volumeSeen := make(map[string]struct{})
	for _, v := range diff.GetVolumeStarts() {
		if err := validateRuntimeID("volume ID", v.GetVolumeId()); err != nil {
			return err
		}
		if _, dup := volumeSeen[v.GetVolumeId()]; dup {
			return fmt.Errorf("duplicate diff volume %q", v.GetVolumeId())
		}
		volumeSeen[v.GetVolumeId()] = struct{}{}
	}
	for _, id := range diff.GetVolumeStops() {
		if err := validateRuntimeID("volume ID", id); err != nil {
			return err
		}
		if _, dup := volumeSeen[id]; dup {
			return fmt.Errorf("duplicate diff volume %q", id)
		}
		volumeSeen[id] = struct{}{}
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
		var accepted agentv1.DesiredNodeState
		if err := proto.Unmarshal(tx.Bucket(localDesiredBucket).Get(desiredStateKey), &accepted); err != nil {
			return fmt.Errorf("decode accepted desired state: %w", err)
		}
		desiredIDs := make(map[string]struct{}, len(accepted.GetServices()))
		for _, service := range accepted.GetServices() {
			desiredIDs[service.GetAllocationId()] = struct{}{}
		}
		if err := applyReportToAllocations(tx.Bucket(localAllocationsBucket), next, desiredIDs); err != nil {
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
		result.LocalStoreID = string(meta.Get(localStoreIDKey))
		result.Initialization = initializationState(meta.Get(initStateKey))
		result.AgentIdentity = string(meta.Get(agentIdentityKey))
		result.ClusterIdentity = string(meta.Get(clusterIdentityKey))
		result.AuthorityEpoch = readUint64(meta.Get(authorityEpochKey))
		result.ReconciliationCursor = readInt64(meta.Get(cursorKey))
		result.NodeConfigVersion = string(meta.Get(nodeConfigVersionKey))
		result.CredentialsVersion = string(meta.Get(credentialsVersionKey))
		result.ReplicasVersion = string(meta.Get(replicasVersionKey))
		if raw := tx.Bucket(localDesiredBucket).Get(desiredStateKey); len(raw) > 0 {
			var desired agentv1.DesiredNodeState
			if err := proto.Unmarshal(raw, &desired); err != nil {
				return err
			}
			result.ObservationOverlayVersion = reconciliation.HashObservationOverlay(desired.GetServices())
		}
		if err := tx.Bucket(localAllocationsBucket).ForEach(func(_, value []byte) error {
			var a localAllocationState
			if err := json.Unmarshal(value, &a); err != nil {
				return err
			}
			result.Allocations = append(result.Allocations, &agentv1.ServiceCondition{
				AllocationId: a.AllocationID, DesiredSpecRevision: a.DesiredSpecRevision,
				AppliedSpecRevision: a.AppliedSpecRevision, DesiredRolloutGeneration: a.DesiredGeneration,
				AppliedRolloutGeneration: a.AppliedGeneration, Phase: a.Phase,
			})
			return nil
		}); err != nil {
			return err
		}
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

// canonicalizeDesiredState orders service and volume lists by their stable
// IDs. Desired configuration is a set; wire order is not semantic. Checkpoints
// arrive in control-plane assignment order while diff application merges via
// maps, so every stored and compared form is canonicalized to keep equality
// and repair comparisons order-independent.
func canonicalizeDesiredState(state *agentv1.DesiredNodeState) {
	sort.Slice(state.GetServices(), func(i, j int) bool {
		return state.GetServices()[i].GetAllocationId() < state.GetServices()[j].GetAllocationId()
	})
	sort.Slice(state.GetVolumes(), func(i, j int) bool {
		return state.GetVolumes()[i].GetVolumeId() < state.GetVolumes()[j].GetVolumeId()
	})
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
	left.SessionId, right.SessionId = "", ""
	left.AuthorityNotAfter, right.AuthorityNotAfter = nil, nil
	// Node configuration is an independently versioned stream, not part of
	// the cursor-versioned allocation configuration: it may legitimately
	// change at the same reconciliation cursor (a repair checkpoint delivers
	// the latest). Its integrity is bound by the content-hash version checked
	// in validateDesiredState, not by the allocation cursor.
	left.NodeConfig, right.NodeConfig = nil, nil
	left.NodeConfigVersion, right.NodeConfigVersion = "", ""
	// The observation overlay (internal hosts, restart observations) derives
	// from live control-plane observations and may likewise change at the same
	// cursor. Its integrity rides the fenced allocation payloads; reconnect
	// drift is detected via the observation overlay version in the hello and
	// repaired with a same-cursor checkpoint.
	for _, svc := range left.GetServices() {
		svc.InternalHosts, svc.RestartObservation = nil, nil
	}
	for _, svc := range right.GetServices() {
		svc.InternalHosts, svc.RestartObservation = nil, nil
	}
	// Configuration is a set of allocations and volumes; wire order is not
	// semantic (checkpoints follow assignment order, diff merges do not).
	canonicalizeDesiredState(left)
	canonicalizeDesiredState(right)
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
	for _, service := range state.GetServices() {
		allocation, err := readAllocation(bucket, service.GetAllocationId())
		if err != nil {
			return err
		}
		allocation.AllocationID = service.GetAllocationId()
		allocation.DesiredSpecRevision = service.GetDesiredSpecRevision()
		allocation.DesiredGeneration = service.GetDesiredRolloutGeneration()
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

func applyReportToAllocations(bucket *bbolt.Bucket, report *agentv1.StatusReport, desiredIDs map[string]struct{}) error {
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
		if err := writeAllocation(bucket, allocation); err != nil {
			return err
		}
	}
	var completed []localAllocationState
	if err := bucket.ForEach(func(key, value []byte) error {
		if _, ok := seen[string(key)]; ok {
			return nil
		}
		if _, ok := desiredIDs[string(key)]; ok {
			return nil
		}
		var allocation localAllocationState
		if err := json.Unmarshal(value, &allocation); err != nil {
			return err
		}
		allocation.RuntimeID = ""
		allocation.Phase = "Stopped"
		completed = append(completed, allocation)
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

func (s *localStateStore) requireClusterIdentity(clusterID string) error {
	var recoveryErr error
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if reason := meta.Get(identityRecoveryKey); len(reason) != 0 {
			recoveryErr = errors.New(string(reason))
			return nil
		}
		if err := validateClusterIdentity(meta, clusterID); err != nil {
			recoveryErr = fmt.Errorf("identity recovery required: %w", err)
			if err := meta.Put(identityRecoveryKey, []byte(recoveryErr.Error())); err != nil {
				return err
			}
			return meta.Put(initStateKey, []byte(initializationRecovery))
		}
		return nil
	})
	return errors.Join(err, recoveryErr)
}

// clusterIdentity returns the pinned cluster identity adopted at
// enrollment, or empty before the first enrollment.
func (s *localStateStore) clusterIdentity() string {
	var id string
	_ = s.db.View(func(tx *bbolt.Tx) error {
		id = string(tx.Bucket(localMetaBucket).Get(clusterIdentityKey))
		return nil
	})
	return id
}

// adoptClusterIdentity replaces the pinned cluster identity with the one a
// certificate renewal observed. Renewal runs over a channel authenticated
// by the pinned roots, so the new identity is trust-continuous; it still
// refuses while the store needs identity recovery.
func (s *localStateStore) adoptClusterIdentity(clusterID string) error {
	if strings.TrimSpace(clusterID) == "" {
		return errors.New("adopted cluster identity is empty")
	}
	var recoveryErr error
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		if reason := meta.Get(identityRecoveryKey); len(reason) != 0 {
			recoveryErr = errors.New(string(reason))
			return nil
		}
		return meta.Put(clusterIdentityKey, []byte(clusterID))
	})
	return errors.Join(err, recoveryErr)
}

func (s *localStateStore) observeAuthorityEpoch(epoch uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		highest := max(readUint64(meta.Get(highestEpochKey)), readUint64(meta.Get(authorityEpochKey)))
		if epoch < highest {
			return fmt.Errorf("stale authority epoch %d follows %d", epoch, highest)
		}
		return putUint64(meta, highestEpochKey, epoch)
	})
}

func (s *localStateStore) nextSessionIncarnation() (uint64, error) {
	var incarnation uint64
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(localMetaBucket)
		previous := readUint64(meta.Get(sessionIncarnationKey))
		if previous >= math.MaxInt64 {
			return errors.New("session incarnation exhausted")
		}
		incarnation = previous + 1
		return putUint64(meta, sessionIncarnationKey, incarnation)
	})
	return incarnation, err
}
