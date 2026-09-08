package source

import (
	"database/sql"
	"time"
)

type GitHubInstallationRecord struct {
	InstallationID                        int64
	AccountLogin, AccountType, TargetType string
	Active                                bool
	CreatedAt, UpdatedAt                  time.Time
}

type GitHubRepositoryRecord struct {
	InstallationID, RepositoryID int64
	Owner, Repo, FullName        string
	Private                      bool
	DefaultBranch                string
	CreatedAt, UpdatedAt         time.Time
}

type GitHubRepositorySnapshotRecord struct {
	RepositoryID          int64
	Owner, Repo, FullName string
	Private               bool
	DefaultBranch         string
	Deleted               bool
	CreatedAt, UpdatedAt  time.Time
}

type GitHubRepositoryView struct {
	RepositoryID                      int64
	Owner, Repo, FullName             string
	Private                           bool
	DefaultBranch                     string
	InstallationID                    int64
	AccessState                       string
	Deleted                           bool
	SnapshotUpdatedAt, GrantUpdatedAt time.Time
}

type GitHubWebhookDeliveryRecord struct {
	ID, DeliveryID, EventType, State, ProcessorID string
	Payload                                       []byte
	LastError                                     string
	ReceivedAt, UpdatedAt                         time.Time
	ProcessedAt                                   sql.NullTime
}
