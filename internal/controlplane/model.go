package controlplane

import (
	"database/sql"
	"time"
)

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

type serviceRolloutRecord struct {
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	Reason            string
	BuildID           string
	RequestedByUserID string
	CreatedAt         time.Time
}
