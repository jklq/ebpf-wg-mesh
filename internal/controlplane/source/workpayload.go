package source

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/durablework"
)

// WorkLeaseTTL bounds how long one source-work claim may run before another replica takes it
// over. Five minutes is headroom for a few GitHub reads plus transactions.
const WorkLeaseTTL = 5 * time.Minute

// SourceWorkAttemptLimit bounds total claims per record, including lease takeovers. Exhausted
// records dead-letter for operator inspection; the next refresh, spec write, or push re-enqueues
// the same dedup key and resurrects the record to pending.
const SourceWorkAttemptLimit = 25

// WorkPayload is the source-layer body carried opaquely in a durable-work record's JSON
// payload. Field names match the retired source_work_items columns.
type WorkPayload struct {
	ServiceID                    string             `json:"service_id,omitempty"`
	SpecRevision                 int64              `json:"spec_revision,omitempty"`
	Provider                     string             `json:"provider,omitempty"`
	ProviderRepositoryExternalID string             `json:"provider_repository_external_id,omitempty"`
	ProviderScopeExternalID      string             `json:"provider_scope_external_id,omitempty"`
	TrackedRef                   string             `json:"tracked_ref,omitempty"`
	CommitSHA                    string             `json:"commit_sha,omitempty"`
	PreviousCommitSHA            string             `json:"previous_commit_sha,omitempty"`
	CommitMessage                string             `json:"commit_message,omitempty"`
	CommitContributors           CommitContributors `json:"commit_contributors,omitempty"`
}

// EncodeWorkPayload renders p as queue payload JSON.
func EncodeWorkPayload(p WorkPayload) ([]byte, error) {
	return json.Marshal(p)
}

// DecodeWorkPayload parses queue payload JSON. Unknown fields are ignored;
// a corrupt payload is a poison record the caller must fail without retry.
func DecodeWorkPayload(raw []byte) (WorkPayload, error) {
	var p WorkPayload
	if len(bytesTrimSpace(raw)) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return WorkPayload{}, fmt.Errorf("decode source work payload: %w", err)
	}
	return p, nil
}

// SourceSpecChangedParams builds the enqueue params for a service source sync. The dedup key
// pins the service revision so concurrent writes converge; force adds a unique suffix for retries.
func SourceSpecChangedParams(serviceID string, specRevision int64, force bool) durablework.EnqueueParams {
	key := fmt.Sprintf("%s:%s:%d", SourceWorkKindSourceSpecChanged, serviceID, specRevision)
	if force {
		key = fmt.Sprintf("%s:%d", key, time.Now().UTC().UnixNano())
	}
	payload, _ := EncodeWorkPayload(WorkPayload{ServiceID: serviceID, SpecRevision: specRevision})
	return durablework.EnqueueParams{
		Kind:         SourceWorkKindSourceSpecChanged,
		DedupKey:     key,
		ResourceType: "service",
		ResourceID:   serviceID,
		Payload:      payload,
		AttemptLimit: SourceWorkAttemptLimit,
	}
}

// SourceResyncParams builds the enqueue params for an unanchored resync after an installation
// refresh. It carries no spec revision: every refresh converges on one active record.
func SourceResyncParams(serviceID string) durablework.EnqueueParams {
	payload, _ := EncodeWorkPayload(WorkPayload{ServiceID: serviceID})
	return durablework.EnqueueParams{
		Kind:         SourceWorkKindSourceSpecChanged,
		DedupKey:     fmt.Sprintf("%s:%s", SourceWorkKindSourceSpecChanged, serviceID),
		ResourceType: "service",
		ResourceID:   serviceID,
		Payload:      payload,
		AttemptLimit: SourceWorkAttemptLimit,
	}
}

// ProviderAccessChangedParams builds the enqueue params for a GitHub installation refresh.
// Concurrent refreshes for one installation converge on one active record.
func ProviderAccessChangedParams(installationID int64) durablework.EnqueueParams {
	scope := ScopeExternalID(installationID)
	payload, _ := EncodeWorkPayload(WorkPayload{Provider: "github", ProviderScopeExternalID: scope})
	return durablework.EnqueueParams{
		Kind:         SourceWorkKindProviderAccessChanged,
		DedupKey:     fmt.Sprintf("%s:github:%d", SourceWorkKindProviderAccessChanged, installationID),
		ResourceType: "github_installation",
		ResourceID:   scope,
		Payload:      payload,
		AttemptLimit: SourceWorkAttemptLimit,
	}
}

// RevisionObservedParams builds the enqueue params for one observed commit. Duplicate
// deliveries converge on one record; the predecessor is part of the identity because the same
// commit can re-arrive via force-push with a different "before" — a different transition.
// PreviousCommitSHA carries the push payload's "before" so the build can prove currency.
func RevisionObservedParams(repositoryExternalID, trackedRef, commitSHA, previousCommitSHA, commitMessage string, contributors CommitContributors) durablework.EnqueueParams {
	payload, _ := EncodeWorkPayload(WorkPayload{
		Provider:                     "github",
		ProviderRepositoryExternalID: strings.TrimSpace(repositoryExternalID),
		TrackedRef:                   strings.TrimSpace(trackedRef),
		CommitSHA:                    strings.TrimSpace(commitSHA),
		PreviousCommitSHA:            strings.TrimSpace(previousCommitSHA),
		CommitMessage:                strings.TrimSpace(commitMessage),
		CommitContributors:           contributors,
	})
	return durablework.EnqueueParams{
		Kind: SourceWorkKindRevisionObserved,
		DedupKey: fmt.Sprintf("%s:github:%s:%s:%s:%s", SourceWorkKindRevisionObserved,
			strings.TrimSpace(repositoryExternalID), strings.TrimSpace(trackedRef),
			strings.TrimSpace(commitSHA), strings.TrimSpace(previousCommitSHA)),
		ResourceType: "github_repository",
		ResourceID:   strings.TrimSpace(repositoryExternalID),
		Payload:      payload,
		AttemptLimit: SourceWorkAttemptLimit,
	}
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
