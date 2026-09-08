package controlplane

import (
	"ebof-wg-mesh/internal/controlplane/source"
	"time"
)

type githubInstallationRecord = source.GitHubInstallationRecord
type githubRepositoryRecord = source.GitHubRepositoryRecord
type githubRepositorySnapshotRecord = source.GitHubRepositorySnapshotRecord
type GitHubRepositoryView = source.GitHubRepositoryView
type githubWebhookDeliveryRecord = source.GitHubWebhookDeliveryRecord

type serviceRolloutRecord struct {
	ServiceID         string
	RolloutGeneration int64
	SpecRevision      int64
	Reason            string
	BuildID           string
	RequestedByUserID string
	CreatedAt         time.Time
}
