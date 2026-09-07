package delivery

import (
	"google.golang.org/protobuf/proto"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
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
		QueuedAt:      Ts(rec.QueuedAt),
		FailureReason: rec.FailureReason,
		CommitMessage: rec.CommitMessage,
		CommitAuthor:  rec.CommitAuthor,
	}
	if rec.StartedAt.Valid {
		status.StartedAt = Ts(rec.StartedAt.Time)
	}
	if rec.FinishedAt.Valid {
		status.FinishedAt = Ts(rec.FinishedAt.Time)
	}
	return status
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

func toProtoResolvedSourceBinding(rec SourceBindingRecord) *platformv1.ResolvedSourceBinding {
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
		AccessState:                  ToProtoSourceAccessState(rec.AccessState),
		ResolvedAt:                   Ts(rec.ResolvedAt),
		FreshUntil:                   Ts(rec.FreshUntil),
		BuildRecipe:                  CloneBuildRecipe(rec.BuildRecipe),
	}
}

func toProtoSourceRevision(rec SourceRevisionRecord) *platformv1.SourceRevision {
	if rec.ID == "" {
		return nil
	}
	return &platformv1.SourceRevision{
		Id:                           rec.ID,
		SourceBindingId:              rec.SourceBindingID,
		ProviderRepositoryExternalId: rec.ProviderRepositoryExternalID,
		TrackedRef:                   rec.TrackedRef,
		CommitSha:                    rec.CommitSHA,
		ObservedAt:                   Ts(rec.ObservedAt),
	}
}

func toProtoSourceSnapshot(rec SourceSnapshotRecord) *platformv1.SourceSnapshot {
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
		snapshot.FetchedAt = Ts(rec.FetchedAt.Time)
	}
	return snapshot
}

func toProtoSourceStateSummary(desired *platformv1.ServiceSourceSpec, binding *SourceBindingRecord, revision *SourceRevisionRecord, snapshot *SourceSnapshotRecord) *platformv1.ServiceSourceSummary {
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
