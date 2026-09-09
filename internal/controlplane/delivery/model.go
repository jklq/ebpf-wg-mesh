package delivery

import (
	"database/sql"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type ProjectKind string

const (
	ProjectKindUser    ProjectKind = "user"
	ProjectKindManaged ProjectKind = "managed"
)

type ProjectRecord struct {
	ID        string
	Name      string
	Kind      ProjectKind
	SystemKey string
	CreatedAt time.Time
}

type EnvironmentKind string

const EnvironmentKindPersistent EnvironmentKind = "persistent"

type EnvironmentRecord struct {
	ID                      string
	ProjectID               string
	Name                    string
	Kind                    EnvironmentKind
	IsProduction            bool
	NetworkIdentity         uint32
	CopiedFromEnvironmentID string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type VolumeRecord struct {
	ID            string
	EnvironmentID string
	Name          string
	SizeBytes     int64
	CreatedAt     time.Time
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
	ResolvedImage           string
	LatestBuildID           string
	LatestBuild             *platformv1.BuildStatus
	LatestDeployment        *DeploymentRecord
	PendingChanges          bool
	UnappliedChanges        []*platformv1.ServiceUnappliedChange
	DesiredReplicaCount     int32
	ReadyReplicaCount       int32
	PlacementMessage        string
	CreatedAt               time.Time
	UpdatedAt               time.Time
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
// mutates this record; effective unavailability is composed with AgentPresence.
type AgentAdministration struct {
	AgentID             string
	LifecycleState      AgentLifecycleState
	OperatorIntent      string
	MaintenanceMessage  string
	CredentialRevokedAt sql.NullTime
	UpdatedAt           time.Time
}

// AgentPresence is owned by the live authenticated session. SessionID fences
// traffic from prior process incarnations.
type AgentPresence struct {
	AgentID                 string
	SessionID               string
	LastObservationSequence uint64
	LastContactAt           time.Time
	Ready                   bool
	Reachable               bool
	UpdatedAt               time.Time
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

// AllocationAssignment is scheduler-owned desired state. An allocation is
// immutable with respect to its agent and addresses.
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

// AllocationObservation is assigned-agent-owned runtime state. Observations
// are retained per generation; only the assignment's current generation can
// contribute readiness.
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
	ImageDigest             string
	FailureReason           string
	SourceRevisionID        string
	SourceSnapshotID        string
	SourceSnapshotDigest    string
	TargetRolloutGeneration int64
	BuildRecipe             *platformv1.BuildRecipe
	QueuedAt                time.Time
	StartedAt               sql.NullTime
	FinishedAt              sql.NullTime
}

type DeploymentRecord struct {
	ID                string
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Build             *BuildRunRecord
	IsCurrent         bool
	RequestedByUserID string
	BuildID           string
	ImageDigest       string
	State             string
	CauseKind         string
	CauseID           string
	ReasonCode        string
	Detail            string
	ResolvedSpec      *platformv1.ServiceSpec
	VariableVersions  map[string]int64
	Transitions       []DeploymentTransitionRecord
	Actions           []DeploymentActionRecord
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
	ImageDigest       string
	RolloutGeneration int64
	OccurredAt        time.Time
}

func (a AgentRecord) Healthy(now time.Time) bool {
	return a.LifecycleState != AgentStateUnavailable && a.LifecycleState != AgentStateRetired && now.Sub(a.LastSeenAt) < AgentHealthyTTL
}
