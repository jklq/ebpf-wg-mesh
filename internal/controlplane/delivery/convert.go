package delivery

import (
	"google.golang.org/protobuf/proto"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/source"
)

func ToProtoBuildStatus(rec BuildRunRecord) *platformv1.BuildStatus {
	if rec.ID == "" {
		return nil
	}
	status := &platformv1.BuildStatus{
		BuildId:       rec.ID,
		State:         toProtoBuildState(rec.State),
		CommitSha:     rec.CommitSHA,
		ImageDigest:   rec.ImageDigest,
		QueuedAt:      ts(rec.QueuedAt),
		FailureReason: rec.FailureReason,
		CommitMessage: rec.CommitMessage,
		CommitAuthor:  rec.CommitAuthor,
		Builder:       rec.BuildRecipe.GetBuilder(),
		AttemptCount:  rec.AttemptCount,
		AttemptLimit:  rec.AttemptLimit,
		BuilderId:     rec.BuilderID,
	}
	if rec.StartedAt.Valid {
		status.StartedAt = ts(rec.StartedAt.Time)
	}
	if rec.FinishedAt.Valid {
		status.FinishedAt = ts(rec.FinishedAt.Time)
	}
	if rec.LeaseExpiresAt.Valid {
		status.LeaseExpiresAt = ts(rec.LeaseExpiresAt.Time)
	}
	if rec.CancelRequestedAt.Valid {
		status.CancelRequestedAt = ts(rec.CancelRequestedAt.Time)
	}
	return status
}

func ToProtoBuildAttempt(rec BuildAttemptRecord) *platformv1.BuildAttempt {
	attempt := &platformv1.BuildAttempt{
		AttemptNumber: rec.AttemptNumber,
		BuilderId:     rec.BuilderID,
		OwnerEpoch:    rec.OwnerEpoch,
		StartedAt:     ts(rec.StartedAt),
		Outcome:       rec.Outcome,
		Detail:        rec.Detail,
	}
	if rec.FinishedAt.Valid {
		attempt.FinishedAt = ts(rec.FinishedAt.Time)
	}
	return attempt
}

func ToProtoBuilderWorker(rec BuilderWorkerRecord) *platformv1.BuilderWorker {
	return &platformv1.BuilderWorker{
		Id:              rec.ID,
		Name:            rec.Name,
		CurrentBuildId:  rec.CurrentBuildID,
		LastHeartbeatAt: ts(rec.LastHeartbeat),
		Drained:         rec.Drained,
		UpdatedAt:       ts(rec.UpdatedAt),
	}
}

func ToProtoBuildSchedulerState(rec BuildSchedulerState) *platformv1.BuildSchedulerState {
	return &platformv1.BuildSchedulerState{
		Paused:                  rec.Paused,
		RunningBuilds:           rec.RunningBuilds,
		QueuedBuilds:            rec.QueuedBuilds,
		MaxConcurrentGlobal:     int32(rec.MaxConcurrentGlobal),
		MaxConcurrentPerProject: int32(rec.MaxConcurrentPerProject),
		UpdatedAt:               ts(rec.UpdatedAt),
	}
}

func toProtoBuildState(state string) platformv1.BuildState {
	switch state {
	case "queued":
		return platformv1.BuildState_BUILD_STATE_QUEUED
	case "running":
		return platformv1.BuildState_BUILD_STATE_RUNNING
	case "succeeded":
		return platformv1.BuildState_BUILD_STATE_SUCCEEDED
	case "failed":
		return platformv1.BuildState_BUILD_STATE_FAILED
	case "superseded":
		return platformv1.BuildState_BUILD_STATE_SUPERSEDED
	case "cancelled":
		return platformv1.BuildState_BUILD_STATE_CANCELLED
	default:
		return platformv1.BuildState_BUILD_STATE_UNSPECIFIED
	}
}

func toProtoResolvedSourceBinding(rec source.SourceBindingRecord) *platformv1.ResolvedSourceBinding {
	if rec.ID == "" {
		return nil
	}
	return &platformv1.ResolvedSourceBinding{
		Id:                           rec.ID,
		ServiceId:                    rec.ServiceID,
		Provider:                     rec.Provider,
		RepositorySelector:           rec.RepositorySelector,
		TrackedRef:                   rec.TrackedRef,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		ProviderScopeExternalId:      rec.ProviderScopeExternalID,
		AccessState:                  source.ToProtoSourceAccessState(rec.AccessState),
		ResolvedAt:                   ts(rec.ResolvedAt),
		FreshUntil:                   ts(rec.FreshUntil),
		BuildRecipe:                  source.CloneBuildRecipe(rec.BuildRecipe),
	}
}

func toProtoSourceRevision(rec source.SourceRevisionRecord) *platformv1.SourceRevision {
	if rec.ID == "" {
		return nil
	}
	return &platformv1.SourceRevision{
		Id:                           rec.ID,
		SourceBindingId:              rec.SourceBindingID,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		TrackedRef:                   rec.TrackedRef,
		CommitSha:                    rec.CommitSHA,
		ObservedAt:                   ts(rec.ObservedAt),
	}
}

func toProtoSourceSnapshot(rec source.SourceSnapshotRecord) *platformv1.SourceSnapshot {
	if rec.ID == "" {
		return nil
	}
	snapshot := &platformv1.SourceSnapshot{
		Id:                           rec.ID,
		SourceRevisionId:             rec.SourceRevisionID,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		CommitSha:                    rec.CommitSHA,
		Digest:                       rec.Digest,
		Ready:                        rec.Ready,
	}
	if rec.FetchedAt.Valid {
		snapshot.FetchedAt = ts(rec.FetchedAt.Time)
	}
	return snapshot
}

func toProtoSourceStateSummary(desired *platformv1.ServiceSourceSpec, binding *source.SourceBindingRecord, revision *source.SourceRevisionRecord, snapshot *source.SourceSnapshotRecord) *platformv1.ServiceSourceSummary {
	if desired == nil {
		return nil
	}
	state := &platformv1.SourceStateSummary{
		DesiredSpec: proto.Clone(desired).(*platformv1.ServiceSourceSpec),
	}
	if binding != nil {
		state.ResolvedBinding = toProtoResolvedSourceBinding(*binding)
	}
	if revision != nil {
		state.LatestRevision = toProtoSourceRevision(*revision)
	}
	if snapshot != nil {
		state.LatestSnapshot = toProtoSourceSnapshot(*snapshot)
	}
	return &platformv1.ServiceSourceSummary{
		Source: &platformv1.ServiceSourceSummary_SourceState{
			SourceState: state,
		},
	}
}
