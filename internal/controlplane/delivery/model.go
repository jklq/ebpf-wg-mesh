package delivery

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/source"

	"google.golang.org/protobuf/encoding/protojson"
)

type ProjectKind string

const (
	ProjectKindUser    ProjectKind = authz.UserProjectKind
	ProjectKindManaged ProjectKind = "managed"
)

// Tombstone is the raw deletion marker stored on a resource row; NULL means never tombstoned.
type Tombstone struct {
	DeletedAt       sql.NullTime
	DeletedByUserID string
	ExpiresAt       sql.NullTime
}

func (t Tombstone) Active() bool {
	return t.DeletedAt.Valid
}

// DeletionInfo is the effective deletion state visible for a resource: its
// own tombstone, or the nearest ancestor tombstone when Inherited is true.
type DeletionInfo struct {
	DeletedAt       time.Time
	DeletedByUserID string
	ExpiresAt       time.Time
	Inherited       bool
}

// EffectiveDeletion resolves deletion state from a resource's own tombstone plus ancestors
// nearest-first. The nearest active tombstone wins; ancestors mark the result inherited.
func EffectiveDeletion(self Tombstone, ancestors ...Tombstone) *DeletionInfo {
	if self.Active() {
		return &DeletionInfo{
			DeletedAt:       self.DeletedAt.Time,
			DeletedByUserID: self.DeletedByUserID,
			ExpiresAt:       self.ExpiresAt.Time,
		}
	}
	for _, ancestor := range ancestors {
		if ancestor.Active() {
			return &DeletionInfo{
				DeletedAt:       ancestor.DeletedAt.Time,
				DeletedByUserID: ancestor.DeletedByUserID,
				ExpiresAt:       ancestor.ExpiresAt.Time,
				Inherited:       true,
			}
		}
	}
	return nil
}

// ScanTombstone appends tombstone scan targets for one table. Columns must be
// selected as deleted_at, deleted_by_user_id, delete_expires_at.
func ScanTombstone(targets []any, tombstone *Tombstone) []any {
	return append(targets, &tombstone.DeletedAt, &tombstone.DeletedByUserID, &tombstone.ExpiresAt)
}

type ProjectRecord struct {
	ID        string
	Name      string
	Kind      ProjectKind
	SystemKey string
	CreatedAt time.Time
	Deletion  *DeletionInfo
	// LogRetentionDays overrides the platform log-retention default. Zero means the default.
	LogRetentionDays int32
}

type EnvironmentKind string

const EnvironmentKindPersistent EnvironmentKind = "persistent"

type EnvironmentRecord struct {
	ID                      string
	ProjectID               string
	Name                    string
	Kind                    EnvironmentKind
	IsProduction            bool
	AutoDeploy              bool
	NetworkIdentity         uint32
	CopiedFromEnvironmentID string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	Deletion                *DeletionInfo
}

type VolumeRecord struct {
	ID            string
	EnvironmentID string
	Name          string
	SizeBytes     int64
	// AgentID pins the volume to one node; empty until its service first
	// deploys.
	AgentID   string
	AgentName string
	// Staged volumes are an unreleased change: the next environment release
	// commits them, and discarding the drafts that mount them removes them.
	Staged    bool
	CreatedAt time.Time
	Deletion  *DeletionInfo
	Status    VolumeStatus
}

// VolumeStatus is the rendered runtime state of a volume: its node's latest
// report folded with the node's presence and administration.
type VolumeStatus struct {
	State      platformv1.VolumeState
	Message    string
	UsedBytes  int64
	ObservedAt time.Time
}

type ServiceRecord struct {
	ID                      string
	EnvironmentID           string
	ProjectID               string
	Name                    string
	Spec                    *platformv1.ServiceSpec
	SourceSummary           *platformv1.ServiceSourceSummary
	SpecRevision            int64
	RolloutGeneration       int64
	AllocatedAgentID        string
	LastSuccessfulCommitSHA string
	// ResolvedArtifactID is the runtime identity; ResolvedImage is its pinned display form.
	ResolvedArtifactID  string
	ResolvedImage       string
	LatestBuildID       string
	LatestBuild         *platformv1.BuildStatus
	LatestDeployment    *DeploymentRecord
	PendingChanges      bool
	UnappliedChanges    []*platformv1.ServiceUnappliedChange
	DesiredReplicaCount int32
	ReadyReplicaCount   int32
	PlacementMessage    string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Deletion            *DeletionInfo
}

type DomainBindingRecord struct {
	Hostname          string
	ProjectID         string
	EnvironmentID     string
	ServiceID         string
	TargetPort        int32
	PlatformGenerated bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Deletion          *DeletionInfo
}

type AgentRecord struct {
	ID                      string
	Name                    string
	LifecycleState          AgentLifecycleState
	StateBeforeUnavailable  AgentLifecycleState
	Region                  string
	Zone                    string
	FailureDomain           string
	ReservedCPUMillis       int64
	ReservedMemoryMebibytes int64
	AdvertiseAddr           string
	WorkloadIPv4Subnet      string
	WorkloadIPv6Subnet      string
	WireGuardPublicKey      string
	WireGuardListenPort     int
	WireGuardEndpoint       string
	WireGuardIPv6           string
	CPUMillisCapacity       int64
	MemoryMebibytesCapcity  int64
	RuntimeCapabilities     []string
	SoftwareVersion         string
	MaintenanceMessage      string
	CredentialRevokedAt     sql.NullTime
	LastSeenAt              time.Time
}

// AgentAdministration is operator-owned intent; runtime connectivity never mutates it.
type AgentAdministration struct {
	AgentID             string
	LifecycleState      AgentLifecycleState
	OperatorIntent      string
	MaintenanceMessage  string
	CredentialRevokedAt sql.NullTime
	UpdatedAt           time.Time
}

type AgentLifecycleState string

const (
	AgentStateEnrolling   AgentLifecycleState = "enrolling"
	AgentStateActive      AgentLifecycleState = "active"
	AgentStateCordoned    AgentLifecycleState = "cordoned"
	AgentStateDraining    AgentLifecycleState = "draining"
	AgentStateUnavailable AgentLifecycleState = "unavailable"
	AgentStateRetired     AgentLifecycleState = "retired"
)

const AgentHealthyTTL = 30 * time.Second

type AllocationRecord struct {
	ID                       string
	ServiceID                string
	ProjectID                string
	EnvironmentID            string
	AgentID                  string
	DesiredSpecRevision      int64
	AppliedSpecRevision      int64
	DesiredRolloutGeneration int64
	AppliedRolloutGeneration int64
	Phase                    string
	Message                  string
	AllocationIPv4           string
	AllocationIPv6           string
	Healthy                  bool
	HealthyIPv4Ports         []int32
	HealthyIPv6Ports         []int32
	CreatedAt                time.Time
	UpdatedAt                time.Time
	Restart                  *platformv1.RestartObservation
	OperatorRestartNonce     int64
	RolloutState             string
	DrainStartedAt           sql.NullTime
	DrainDeadline            sql.NullTime
}

type AllocationAssignment struct {
	ID                   string
	AgentID              string
	ServiceID            string
	DeploymentID         string
	SpecRevision         int64
	RolloutGeneration    int64
	IPv4                 string
	IPv6                 string
	Intent               string
	IntentMessage        string
	OperatorRestartNonce int64
	RolloutState         string
	DrainStartedAt       sql.NullTime
	DrainDeadline        sql.NullTime
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type AllocationObservation struct {
	AllocationID        string
	RolloutGeneration   int64
	AppliedSpecRevision int64
	AppliedGeneration   int64
	Phase               string
	Message             string
	Healthy             bool
	HealthyIPv4Ports    []int32
	HealthyIPv6Ports    []int32
	Restart             *platformv1.RestartObservation
	AgentID             string
	SessionID           string
	Sequence            uint64
	ObservedAt          time.Time
}

type BuildRunRecord struct {
	ID                      string
	ServiceID               string
	ProjectID               string
	EnvironmentID           string
	CommitSHA               string
	CommitMessage           string
	CommitContributors      source.CommitContributors
	State                   string
	BuilderID               string
	OwnerEpoch              int64
	LeaseExpiresAt          sql.NullTime
	AttemptCount            int64
	AttemptLimit            int64
	CancelRequestedAt       sql.NullTime
	DeadlineAt              sql.NullTime
	LastHeartbeatAt         sql.NullTime
	ArtifactID              string
	ImageDigest             string
	Artifact                *BuildArtifactRecord
	FailureReason           string
	SourceRevisionID        string
	SourceSnapshotID        string
	SourceSnapshotDigest    string
	BuildActorKind          string
	BuildActorID            string
	TargetRolloutGeneration int64
	BuildRecipe             *platformv1.BuildRecipe
	QueuedAt                time.Time
	StartedAt               sql.NullTime
	FinishedAt              sql.NullTime
}

type BuildAttemptRecord struct {
	AttemptNumber int64
	BuilderID     string
	OwnerEpoch    int64
	StartedAt     time.Time
	FinishedAt    sql.NullTime
	Outcome       string
	Detail        string
}

type BuilderWorkerRecord struct {
	ID             string
	Name           string
	CurrentBuildID string
	LastHeartbeat  time.Time
	Drained        bool
	UpdatedAt      time.Time
}

type BuildSchedulerState struct {
	Paused                  bool
	RunningBuilds           int64
	QueuedBuilds            int64
	MaxConcurrentGlobal     int
	MaxConcurrentPerProject int
	UpdatedAt               time.Time
}

type DeploymentRecord struct {
	ID                string
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Build             *BuildRunRecord
	Artifact          *BuildArtifactRecord
	IsCurrent         bool
	RequestedByUserID string
	BuildID           string
	// ArtifactID is the runtime identity; ImageDigest mirrors the artifact's pinned image ref.
	ArtifactID  string
	ImageDigest string
	State       string
	CauseKind   string
	CauseID     string
	ReasonCode  string
	Detail      string
	// ResolvedSpec is the deployment's spec revision, env included. Listings leave it unset.
	ResolvedSpec *platformv1.ServiceSpec
	Transitions  []DeploymentTransitionRecord
	Actions      []DeploymentActionRecord
}

// BuildReused reports whether the deployment reused an earlier deployment's image. Creating
// transitions carry BUILD_REUSED; later transitions overwrite the reason code.
func (d DeploymentRecord) BuildReused() bool {
	if d.ReasonCode == reasonBuildReused {
		return true
	}
	for _, transition := range d.Transitions {
		if transition.ReasonCode == reasonBuildReused {
			return true
		}
	}
	return false
}

type DeploymentActionRecord struct {
	ID                 string
	ServiceID          string
	TargetDeploymentID string
	ResultDeploymentID string
	Action             string
	AllocationID       string
	IdempotencyKey     string
	RequestedByUserID  string
	CreatedAt          time.Time
}

type DeploymentTransitionRecord struct {
	ID                string
	DeploymentID      string
	FromState         string
	ToState           string
	CauseKind         string
	CauseID           string
	ReasonCode        string
	Detail            string
	SpecRevision      int64
	ArtifactID        string
	ImageDigest       string
	RolloutGeneration int64
	OccurredAt        time.Time
}

func (a AgentRecord) Healthy(now time.Time) bool {
	if a.LastSeenAt.IsZero() {
		return false
	}
	return a.LifecycleState != AgentStateUnavailable && a.LifecycleState != AgentStateRetired && now.UTC().Sub(a.LastSeenAt.UTC()) < AgentHealthyTTL
}

var (
	ErrVolumeInUse              = errors.New("volume still referenced by service")
	ErrVolumeNotFound           = errors.New("volume not found")
	ErrVolumeAlreadyExists      = errors.New("volume already exists")
	ErrInvalidVolume            = errors.New("volume name must be lowercase letters, digits, and hyphens")
	ErrInvalidServiceEnv        = errors.New("invalid environment variable")
	ErrLeaseLost                = errors.New("control-plane lease lost")
	ErrInvalidVolumeMount       = errors.New("invalid volume mount")
	ErrVolumeAttached           = errors.New("volume is attached to another service")
	ErrVolumeShrink             = errors.New("volume size can only grow")
	ErrInvalidVolumeSize        = errors.New("invalid volume size")
	ErrConcurrentUpdate         = errors.New("concurrent service update")
	ErrDomainAlreadyExists      = errors.New("domain binding already exists")
	ErrInvalidPort              = errors.New("port must be an integer between 1 and 65535")
	ErrNoPlacementAvailable     = errors.New("no healthy agent satisfies placement")
	ErrInvalidReplicaCount      = errors.New("desired replica count is invalid")
	ErrVolumeReplicaUnsupported = errors.New("volume-backed services support a single replica")
	ErrServiceDeleted           = errors.New("service is deleted")
	ErrEnvironmentDeleted       = errors.New("environment is deleted")
	ErrProjectDeleted           = errors.New("project is deleted")
	ErrDomainDeleted            = errors.New("domain binding is deleted")
	ErrConfirmationMismatch     = errors.New("confirmation name does not match the current resource name")
	ErrAncestorDeleted          = errors.New("cannot restore under a deleted parent; restore the parent first")
	ErrDeletionExpired          = errors.New("deletion grace period has expired")
	ErrServiceAlreadyExists     = errors.New("service already exists")
	ErrEnvironmentAlreadyExists = errors.New("environment already exists")
)

type jsonInt32Slice []int32

type JSONInt32Slice = jsonInt32Slice

type jsonStringSlice []string

func encodeRestartObservation(obs *platformv1.RestartObservation) ([]byte, error) {
	if obs == nil {
		return []byte("{}"), nil
	}
	return protojson.Marshal(obs)
}

func decodeRestartObservation(raw []byte) (*platformv1.RestartObservation, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}
	obs := &platformv1.RestartObservation{}
	if err := protojson.Unmarshal(raw, obs); err != nil {
		return nil, err
	}
	return obs, nil
}

func (p *jsonStringSlice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]string)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]string)(p))
	default:
		return fmt.Errorf("scan string slice json: unsupported type %T", src)
	}
}

func (p *jsonInt32Slice) Scan(src any) error {
	if p == nil {
		return nil
	}
	switch v := src.(type) {
	case nil:
		*p = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]int32)(p))
	case string:
		return json.Unmarshal([]byte(v), (*[]int32)(p))
	default:
		return fmt.Errorf("scan int32 slice json: unsupported type %T", src)
	}
}
