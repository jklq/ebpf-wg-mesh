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

type principalRecord struct {
	Subject   string
	Email     string
	CreatedAt time.Time
}

type projectRecord struct {
	ID        string
	Name      string
	Kind      projectKind
	SystemKey string
	CreatedAt time.Time
}

type volumeRecord struct {
	ID           string
	ProjectID    string
	Name         string
	SizeBytes    int64
	BoundAgentID string
	CreatedAt    time.Time
}

type serviceRecord struct {
	ID                      string
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
	PendingChanges          bool
	UnappliedChanges        []*platformv1.ServiceUnappliedChange
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type domainBindingRecord struct {
	Hostname   string
	ProjectID  string
	ServiceID  string
	TargetPort int32
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type agentRecord struct {
	ID                     string
	Name                   string
	AdvertiseAddr          string
	WorkloadIPv6Subnet     string
	WireGuardPublicKey     string
	WireGuardListenPort    int
	WireGuardIPv6          string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	LastSeenAt             time.Time
}

func (a agentRecord) healthy(now time.Time) bool {
	return now.Sub(a.LastSeenAt) < 30*time.Second
}

type allocationRecord struct {
	ID                       string
	ServiceID                string
	ProjectID                string
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
	UpdatedAt                time.Time
}

type buildRunRecord struct {
	ID                      string
	ServiceID               string
	ProjectID               string
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
	ArchiveTGZ                   []byte
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
	ServiceID          string
	RolloutGeneration  int64
	SpecRevision       int64
	Reason             string
	BuildID            string
	RequestedBySubject string
	RequestedByEmail   string
	CreatedAt          time.Time
}

type deploymentRecord struct {
	ID                 string
	ServiceID          string
	RolloutGeneration  int64
	SpecRevision       int64
	Reason             string
	CreatedAt          time.Time
	Build              *buildRunRecord
	IsCurrent          bool
	RequestedBySubject string
	RequestedByEmail   string
}
