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

type SourceWorkItemRecord struct {
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

const (
	SourceAccessStateAvailable            = "available"
	SourceAccessStateInstallationRequired = "installation_required"
	SourceAccessStateAccessRevoked        = "access_revoked"
	SourceAccessStateRepositoryDeleted    = "repository_deleted"

	SourceWorkKindSourceSpecChanged     = "source_spec_changed"
	SourceWorkKindProviderAccessChanged = "provider_access_changed"
	SourceWorkKindRevisionObserved      = "revision_observed"

	SourceWorkStatePending    = "pending"
	SourceWorkStateProcessing = "processing"
)

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
}

type QueuedBuild struct {
	BuildID string
}
