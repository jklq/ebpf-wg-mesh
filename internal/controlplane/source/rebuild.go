package source

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/durablework"
)

// RebuildRequest pins historical source independently of the tracked branch head.
type RebuildRequest struct {
	DeploymentID       string
	ServiceID          string
	ProjectID          string
	RepositorySelector string
	Revision           SourceRevisionRecord
	BuildRecipe        *platformv1.BuildRecipe
}

func DeploymentRebuildParams(deploymentID, serviceID, revisionID string) durablework.EnqueueParams {
	payload, _ := EncodeWorkPayload(WorkPayload{ServiceID: serviceID, DeploymentID: deploymentID, SourceRevisionID: revisionID})
	return durablework.EnqueueParams{
		Kind: SourceWorkKindDeploymentRebuild, DedupKey: SourceWorkKindDeploymentRebuild + ":" + deploymentID,
		ResourceType: "service", ResourceID: serviceID, Payload: payload, AttemptLimit: SourceWorkAttemptLimit,
	}
}

func (c *GitHubCoordinator) rebuildDeployment(ctx context.Context, payload WorkPayload) error {
	req, active, err := c.delivery.DeploymentRebuildRequest(ctx, payload.DeploymentID, payload.SourceRevisionID)
	if err != nil || !active {
		return err
	}
	pending, err := c.prepareSnapshot(ctx, req.ProjectID, req.RepositorySelector, req.Revision)
	if err != nil {
		// Refetch is best effort. Fail the deployment visibly; keep its serving predecessor.
		if failErr := c.delivery.FailDeploymentRebuild(ctx, req.DeploymentID, err.Error()); failErr != nil {
			return failErr
		}
		return err
	}
	if err := c.delivery.QueueDeploymentRebuild(ctx, req, pending); err != nil {
		if failErr := c.delivery.FailDeploymentRebuild(ctx, req.DeploymentID, err.Error()); failErr != nil {
			return failErr
		}
		return err
	}
	return nil
}

func (c *GitHubCoordinator) prepareSnapshot(ctx context.Context, projectID, selector string, revision SourceRevisionRecord) (SourceSnapshotRecord, error) {
	if _, err := c.store.SourceSnapshotByRevisionID(ctx, revision.ID); err == nil {
		return SourceSnapshotRecord{}, nil
	} else if err != sql.ErrNoRows {
		return SourceSnapshotRecord{}, err
	}
	owner, repo, err := SplitGitHubRepositorySelector(selector)
	if err != nil {
		return SourceSnapshotRecord{}, err
	}
	installation, err := c.store.ProjectGitHubRepositoryInstallation(ctx, projectID, owner, repo)
	if err != nil {
		return SourceSnapshotRecord{}, refetchError(selector, revision.CommitSHA, err)
	}
	stream, size, err := c.client.FetchArchiveStream(ctx, owner, repo, revision.CommitSHA, installation)
	if err != nil {
		return SourceSnapshotRecord{}, refetchError(selector, revision.CommitSHA, err)
	}
	defer stream.Close()
	digest, key, storedSize, err := c.store.StoreSourceArchiveFromReader(ctx, stream, size)
	if err != nil {
		return SourceSnapshotRecord{}, refetchError(selector, revision.CommitSHA, err)
	}
	return SourceSnapshotRecord{
		SourceRevisionID: revision.ID, Provider: revision.Provider,
		ProviderRepositoryExternalID: revision.ProviderRepositoryExternalID, CommitSHA: revision.CommitSHA,
		Digest: digest, ObjectKey: key, ArchiveSizeBytes: storedSize, Ready: true,
		FetchedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	}, nil
}

func refetchError(repository, commit string, err error) error {
	return fmt.Errorf("cannot fetch GitHub source %s at commit %s: source archives are removed after builds finish; check repository access and whether this commit still exists, then retry: %w", repository, commit, err)
}
