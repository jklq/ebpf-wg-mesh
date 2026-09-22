package delivery

import (
	"database/sql"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
)

type ProjectKind string

const (
	ProjectKindUser    ProjectKind = authz.UserProjectKind
	ProjectKindManaged ProjectKind = "managed"
)

// Tombstone is the raw deletion marker stored on a resource row. A NULL
// deleted_at means the row was never tombstoned.
type Tombstone struct {
	DeletedAt       sql.NullTime
	DeletedByUserID string
	ExpiresAt       sql.NullTime
}

// Active reports whether the tombstone is set.
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

// Expired reports whether the grace period ended at or before cutoff.
func (d *DeletionInfo) Expired(cutoff time.Time) bool {
	return d != nil && !d.ExpiresAt.After(cutoff)
}

// EffectiveDeletion resolves the deletion state from a resource's own
// tombstone plus ancestor tombstones ordered nearest-first. The nearest
// active tombstone wins; ancestors mark the result inherited.
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
	CreatedAt     time.Time
	Deletion      *DeletionInfo
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
	// ResolvedArtifactID is the runtime identity; ResolvedImage is its
	// pinned display form, always equal to the artifact's image ref.
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

// AgentAdministration is operator-owned intent. Runtime connectivity never
// mutates this record; effective unavailability is composed with the live session.
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
	CommitAuthor            string
	State                   string
	BuilderID               string
	OwnerEpoch              int64
	LeaseExpiresAt          sql.NullTime
	AttemptCount            int64
	AttemptLimit            int64
	CancelRequestedAt       sql.NullTime
	CancelRequestedBy       string
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
	ID            string
	BuildID       string
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
	// ArtifactID is the runtime identity; ImageDigest mirrors the
	// artifact's pinned image ref for display.
	ArtifactID       string
	ImageDigest      string
	State            string
	CauseKind        string
	CauseID          string
	ReasonCode       string
	Detail           string
	ResolvedSpec     *platformv1.ServiceSpec
	VariableVersions map[string]int64
	SealedVersions   map[string]int64
	Transitions      []DeploymentTransitionRecord
	Actions          []DeploymentActionRecord
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
