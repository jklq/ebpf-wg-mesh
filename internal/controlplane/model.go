package controlplane

import (
	"database/sql"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type projectKind string

const (
	projectKindUser    projectKind = "user"
	projectKindManaged projectKind = "managed"
)

type projectRecord struct {
	ID        string
	Name      string
	Kind      projectKind
	SystemKey string
	CreatedAt time.Time
}

type environmentKind string

const environmentKindPersistent environmentKind = "persistent"

type environmentRecord struct {
	ID                      string
	ProjectID               string
	Name                    string
	Kind                    environmentKind
	IsProduction            bool
	NetworkIdentity         uint32
	CopiedFromEnvironmentID string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type volumeRecord struct {
	ID            string
	EnvironmentID string
	Name          string
	SizeBytes     int64
	CreatedAt     time.Time
}

type serviceRecord struct {
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
	LatestDeployment        *deploymentRecord
	PendingChanges          bool
	UnappliedChanges        []*platformv1.ServiceUnappliedChange
	DesiredReplicaCount     int32
	ReadyReplicaCount       int32
	PlacementMessage        string
	SandboxProfileAudit     []sandboxProfileAuditRecord
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type domainBindingRecord struct {
	Hostname          string
	ProjectID         string
	EnvironmentID     string
	ServiceID         string
	TargetPort        int32
	PlatformGenerated bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type agentRecord struct {
	ID                      string
	Name                    string
	LifecycleState          agentLifecycleState
	StateBeforeUnavailable  agentLifecycleState
	Region                  string
	Zone                    string
	FailureDomain           string
	ReservedCPUMillis       int64
	ReservedMemoryMebibytes int64
	AdvertiseAddr           string
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

type agentLifecycleState string

const (
	agentStateEnrolling   agentLifecycleState = "enrolling"
	agentStateActive      agentLifecycleState = "active"
	agentStateCordoned    agentLifecycleState = "cordoned"
	agentStateDraining    agentLifecycleState = "draining"
	agentStateUnavailable agentLifecycleState = "unavailable"
	agentStateRetired     agentLifecycleState = "retired"
)

const agentHealthyTTL = 30 * time.Second

func (a agentRecord) healthy(now time.Time) bool {
	return a.LifecycleState != agentStateUnavailable && a.LifecycleState != agentStateRetired && now.Sub(a.LastSeenAt) < agentHealthyTTL
}

type allocationRecord struct {
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
	AllocationIP             string
	Healthy                  bool
	HealthyPorts             []int32
	CreatedAt                time.Time
	UpdatedAt                time.Time
	Restart                  *platformv1.RestartObservation
	OperatorRestartNonce     int64
	RolloutState             string
	DrainStartedAt           sql.NullTime
	DrainDeadline            sql.NullTime
}

type buildRunRecord struct {
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

type githubInstallationRecord struct {
	InstallationID int64
	AccountLogin   string
	AccountType    string
	TargetType     string
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type githubRepositoryRecord struct {
	InstallationID int64
	RepositoryID   int64
	Owner          string
	Repo           string
	FullName       string
	Private        bool
	DefaultBranch  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type githubRepositorySnapshotRecord struct {
	RepositoryID  int64
	Owner         string
	Repo          string
	FullName      string
	Private       bool
	DefaultBranch string
	Deleted       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type GitHubRepositoryView struct {
	RepositoryID      int64
	Owner             string
	Repo              string
	FullName          string
	Private           bool
	DefaultBranch     string
	InstallationID    int64
	AccessState       string
	Deleted           bool
	SnapshotUpdatedAt time.Time
	GrantUpdatedAt    time.Time
}

type githubWebhookDeliveryRecord struct {
	ID          string
	DeliveryID  string
	EventType   string
	State       string
	ProcessorID string
	Payload     []byte
	LastError   string
	ReceivedAt  time.Time
	UpdatedAt   time.Time
	ProcessedAt sql.NullTime
}

type githubWorkItemRecord struct {
	ID             string
	Kind           string
	State          string
	ProcessorID    string
	IdempotencyKey string
	ServiceID      string
	SpecRevision   int64
	InstallationID int64
	CommitSHA      string
	LastError      string
	AttemptCount   int64
	AvailableAt    time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type sourceBindingRecord struct {
	ID                           string
	ServiceID                    string
	ProjectID                    string
	EnvironmentID                string
	Provider                     string
	RepositorySelector           string
	TrackedRef                   string
	ProviderRepositoryExternalID string
	ProviderScopeExternalID      string
	AccessState                  string
	BuildRecipe                  *platformv1.BuildRecipe
	ResolvedAt                   time.Time
	FreshUntil                   time.Time
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

type sourceRevisionRecord struct {
	ID                           string
	SourceBindingID              string
	ServiceID                    string
	Provider                     string
	ProviderRepositoryExternalID string
	TrackedRef                   string
	CommitSHA                    string
	CommitMessage                string
	CommitAuthor                 string
	ObservedAt                   time.Time
	CreatedAt                    time.Time
}

type sourceSnapshotRecord struct {
	ID                           string
	SourceRevisionID             string
	Provider                     string
	ProviderRepositoryExternalID string
	CommitSHA                    string
	Digest                       string
	ObjectKey                    string
	ArchiveSizeBytes             int64
	Ready                        bool
	FetchedAt                    sql.NullTime
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

type sourceWorkItemRecord struct {
	ID                           string
	Kind                         string
	State                        string
	ProcessorID                  string
	IdempotencyKey               string
	ServiceID                    string
	SpecRevision                 int64
	Provider                     string
	ProviderRepositoryExternalID string
	ProviderScopeExternalID      string
	TrackedRef                   string
	CommitSHA                    string
	CommitMessage                string
	CommitAuthor                 string
	LastError                    string
	AttemptCount                 int64
	AvailableAt                  time.Time
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

type serviceRolloutRecord struct {
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	Reason            string
	BuildID           string
	RequestedByUserID string
	CreatedAt         time.Time
}

type deploymentRecord struct {
	ID                string
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	Reason            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Build             *buildRunRecord
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
	Transitions       []deploymentTransitionRecord
	Actions           []deploymentActionRecord
}

type deploymentActionRecord struct {
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

type deploymentTransitionRecord struct {
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
