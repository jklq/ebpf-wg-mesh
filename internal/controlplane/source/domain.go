package source

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type SourceBindingRecord struct {
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

type SourceRevisionRecord struct {
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

type SourceSnapshotRecord struct {
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

const (
	SourceAccessStateAvailable            = "available"
	SourceAccessStateInstallationRequired = "installation_required"
	SourceAccessStateAccessRevoked        = "access_revoked"
	SourceAccessStateRepositoryDeleted    = "repository_deleted"

	SourceWorkKindSourceSpecChanged     = "source_spec_changed"
	SourceWorkKindProviderAccessChanged = "provider_access_changed"
	SourceWorkKindRevisionObserved      = "revision_observed"
)

// SourceWorkKinds lists every durable-work kind the source layer enqueues.
// Claims are restricted to these kinds so other queues sharing the table
// are never picked up by the source reconciler.
var SourceWorkKinds = []string{
	SourceWorkKindSourceSpecChanged,
	SourceWorkKindProviderAccessChanged,
	SourceWorkKindRevisionObserved,
}

func CloneBuildRecipe(recipe *platformv1.BuildRecipe) *platformv1.BuildRecipe {
	if recipe == nil {
		return nil
	}
	return proto.Clone(recipe).(*platformv1.BuildRecipe)
}

func MarshalBuildRecipe(recipe *platformv1.BuildRecipe) ([]byte, error) {
	if recipe == nil {
		return []byte("{}"), nil
	}
	return protojson.Marshal(recipe)
}

func UnmarshalBuildRecipe(raw []byte) (*platformv1.BuildRecipe, error) {
	if len(raw) == 0 || string(raw) == "" || string(raw) == "null" || string(raw) == "{}" {
		return &platformv1.BuildRecipe{}, nil
	}
	recipe := &platformv1.BuildRecipe{}
	if err := protojson.Unmarshal(raw, recipe); err != nil {
		return nil, err
	}
	return recipe, nil
}

func DesiredSourceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSpec {
	if spec == nil || spec.GetSource() == nil {
		return nil
	}
	return spec.GetSource().GetSourceSpec()
}

func EnsureReadySnapshot(snapshot SourceSnapshotRecord) error {
	if snapshot.ID == "" {
		return sql.ErrNoRows
	}
	if !snapshot.Ready || snapshot.ArchiveSizeBytes <= 0 {
		return fmt.Errorf("snapshot %s is not ready", snapshot.ID)
	}
	return nil
}

func ToProtoSourceAccessState(value string) platformv1.SourceAccessState {
	switch strings.TrimSpace(value) {
	case SourceAccessStateAvailable:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_AVAILABLE
	case SourceAccessStateInstallationRequired:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_INSTALLATION_REQUIRED
	case SourceAccessStateAccessRevoked:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_ACCESS_REVOKED
	case SourceAccessStateRepositoryDeleted:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_REPOSITORY_DELETED
	default:
		return platformv1.SourceAccessState_SOURCE_ACCESS_STATE_UNSPECIFIED
	}
}

type Service struct {
	ID           string
	ProjectID    string
	Spec         *platformv1.ServiceSpec
	SpecRevision int64
	// Deleted marks a tombstoned service (or one under a tombstoned
	// ancestor). The coordinator drops work for deleted services.
	Deleted bool
}

// BuildTransition proves a build request's currency against the binding's
// proven head commit. Only current requests are served: those whose commit
// is the head, those that advance from it (PreviousCommit — a push
// payload's "before", including force-pushes back to an older commit), and
// tracked-head syncs that just fetched the ref head (TrackedHead) — but a
// fetch speaks only for its own point in time (FetchedFromHead), so a
// delayed sync can never undo a push that landed while it was in flight.
// A redelivered or retried older revision carries no proof and is refused:
// it must never supersede queued newer work or regress the rollout. Moving
// backward on purpose is the rollback and exact-redeploy actions' job.
type BuildTransition struct {
	PreviousCommit string
	// TrackedHead marks a tracked-head sync: the caller fetched the ref
	// head just now. FetchedFromHead is the proven head it observed before
	// that fetch; the fetch proves the revision current only while that
	// head still holds at apply time. Without that fence a fetch that
	// began before a newer push would move the proven head backward and
	// could supersede the newer push's queued build or roll the old commit
	// out.
	TrackedHead     bool
	FetchedFromHead string
}

// ProvesCurrent reports whether a request carrying t proves itself current
// against the binding's proven head commit: it is the head, it advances
// from the head, or the caller fetched it as the tracked head over a basis
// that still holds — a request with no head established yet establishes
// it. Recording an observation never speaks for currency by itself —
// arrival order is not push order, and a delayed observation of an unseen
// older commit must not become the head.
func (t BuildTransition) ProvesCurrent(revisionCommit, headCommit string) bool {
	if headCommit == "" || headCommit == revisionCommit {
		return true
	}
	if t.TrackedHead {
		return t.FetchedFromHead == headCommit
	}
	return t.PreviousCommit != "" && t.PreviousCommit == headCommit
}

// NoPushPredecessor reports whether a push payload's "before" carries no
// predecessor to chain to: GitHub sends the all-zero SHA when a ref is
// created or recreated. The binding's stored tip predates such a deletion,
// so the zero SHA can never be observed and the transition must prove its
// currency by fetching the tracked head instead (see BuildTransition).
func NoPushPredecessor(previousCommitSHA string) bool {
	sha := strings.TrimSpace(previousCommitSHA)
	return sha == "" || strings.Trim(sha, "0") == ""
}

type QueuedBuild struct {
	// BuildID is the queued build, or the original build whose image was
	// reused when Reused is true.
	BuildID string
	// DeploymentID is the deployment carrying this queue decision.
	DeploymentID string
	// Reused reports that no builder work was queued: the source already
	// produced an image and it was scheduled directly.
	Reused bool
	// Superseded reports that the revision is not the binding's latest
	// observed commit and no work was created: an out-of-order or
	// redelivered revision must never regress the rollout. Superseded
	// reports that no work was created because the request is not
	// current. PendingPredecessor refines it: the request's push
	// transition names a predecessor the binding has not observed yet,
	// so the request is early rather than stale and should be requeued
	// until the predecessor advances the head.
	Superseded         bool
	PendingPredecessor bool
}
