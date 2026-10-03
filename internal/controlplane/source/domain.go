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
	CommitContributors           CommitContributors
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

// SourceWorkKinds lists every durable-work kind the source layer enqueues. Claims are
// restricted to these so other queues sharing the table are never picked up.
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
	// Deleted marks a tombstoned service (or one under a tombstoned ancestor). Work is dropped.
	Deleted bool
}

// BuildTransition proves a build request's currency against the binding's proven head. Only
// current requests are served: the head itself, transitions advancing from it (PreviousCommit,
// including force-pushes back to an older commit), and tracked-head syncs over a basis that
// still holds (FetchedFromHead). Older revisions carry no proof and are refused; moving
// backward on purpose is the rollback and exact-redeploy actions' job.
type BuildTransition struct {
	// History marks a known-stale observation recorded for history only. It can never prove
	// currency or establish the head, so a late event can't become the chain anchor.
	History        bool
	PreviousCommit string
	// TrackedHead marks a tracked-head sync: the caller just fetched the ref head over basis
	// FetchedFromHead. The fetch proves currency only while that head still holds; without the
	// fence a pre-push fetch would move the head backward and supersede newer work.
	TrackedHead     bool
	FetchedFromHead string
}

// ProvesCurrent reports whether a request proves itself current against the proven head: it
// is the head, advances from it, or was fetched as the tracked head over a still-holding
// basis. With no head yet, only a fetch (or a chainless request) establishes it — arrival
// order is not push order, and a delayed push must not install a stale head.
func (t BuildTransition) ProvesCurrent(revisionCommit, headCommit string) bool {
	if t.History {
		return false
	}
	if headCommit == revisionCommit {
		return true
	}
	if t.TrackedHead {
		return t.FetchedFromHead == headCommit
	}
	if headCommit == "" {
		return t.PreviousCommit == ""
	}
	return t.PreviousCommit != "" && t.PreviousCommit == headCommit
}

// NoPushPredecessor reports whether a push "before" carries no chainable predecessor: GitHub
// sends the all-zero SHA when a ref is created or recreated, which can never be observed, so
// the transition must prove currency by fetching the tracked head.
func NoPushPredecessor(previousCommitSHA string) bool {
	sha := strings.TrimSpace(previousCommitSHA)
	return sha == "" || strings.Trim(sha, "0") == ""
}

type QueuedBuild struct {
	// BuildID is the queued build, or the reused image's original build when Reused is true.
	BuildID string
	// DeploymentID is the deployment carrying this queue decision.
	DeploymentID string
	// Reused reports that no builder work was queued: the existing image was scheduled directly.
	Reused bool
	// Superseded reports that no work was created because the request is not current against
	// the binding's proven head. ChainUnproven refines it: a push chaining to neither the proven
	// nor a fetched head, where staleness is not provable — so the tracked head is reconciled and
	// only the commit still current builds.
	Superseded    bool
	ChainUnproven bool
}
