package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

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

func EnsureReadySnapshot(snapshot SourceSnapshotRecord) error {
	if snapshot.ID == "" {
		return sql.ErrNoRows
	}
	if !snapshot.Ready || snapshot.ArchiveSizeBytes <= 0 {
		return fmt.Errorf("snapshot %s is not ready", snapshot.ID)
	}
	return nil
}

func (s *persistence) latestSourceRevisionByBindingIDTx(ctx context.Context, q ServiceQueryer, sourceBindingID string) (SourceRevisionRecord, error) {
	var rec SourceRevisionRecord
	err := q.QueryRowContext(ctx,
		`SELECT id, source_binding_id, service_id, provider, provider_repository_external_id,
		        tracked_ref, commit_sha, commit_message, commit_author, observed_at, created_at
		   FROM source_revisions
		  WHERE source_binding_id = $1
		  ORDER BY observed_at DESC, created_at DESC, id DESC
		  LIMIT 1`,
		sourceBindingID,
	).Scan(
		&rec.ID,
		&rec.SourceBindingID,
		&rec.ServiceID,
		&rec.Provider,
		&rec.ProviderRepositoryExternalID,
		&rec.TrackedRef,
		&rec.CommitSHA,
		&rec.CommitMessage,
		&rec.CommitAuthor,
		&rec.ObservedAt,
		&rec.CreatedAt,
	)
	if err != nil {
		return SourceRevisionRecord{}, err
	}
	return rec, nil
}

func (s *persistence) loadServiceSourceSummaryQuerier(ctx context.Context, q ServiceQueryer, spec *platformv1.ServiceSpec, serviceID string) (*platformv1.ServiceSourceSummary, error) {
	if spec == nil || spec.GetSource() == nil {
		return nil, nil
	}
	if image := spec.GetSource().GetImage(); image != nil {
		return BuildSourceSummary(spec), nil
	}
	desired := DesiredSourceSpec(spec)
	if desired == nil {
		return nil, nil
	}
	binding, err := s.sourceBindingByServiceIDQuerier(ctx, q, serviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, nil, nil, nil), nil
	case err != nil:
		return nil, err
	}
	revision, err := s.latestSourceRevisionByBindingIDTx(ctx, q, binding.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, nil, nil), nil
	case err != nil:
		return nil, err
	}
	snapshot, err := s.sourceSnapshotByRevisionIDTx(ctx, q, revision.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, &revision, nil), nil
	case err != nil:
		return nil, err
	default:
		return toProtoSourceStateSummary(desired, &binding, &revision, &snapshot), nil
	}
}

func (s *persistence) enqueueSourceSpecChangedTx(ctx context.Context, tx *sql.Tx, serviceID string, specRevision int64, force bool) error {
	if serviceID == "" {
		return errors.New("service id is required")
	}
	key := fmt.Sprintf("%s:%s:%d", SourceWorkKindSourceSpecChanged, serviceID, specRevision)
	if force {
		key = fmt.Sprintf("%s:%s:%d:%d", SourceWorkKindSourceSpecChanged, serviceID, specRevision, time.Now().UTC().UnixNano())
	}
	_, err := s.enqueueSourceWorkItemTx(ctx, tx, SourceWorkItemRecord{
		Kind:           SourceWorkKindSourceSpecChanged,
		IdempotencyKey: key,
		ServiceID:      serviceID,
		SpecRevision:   specRevision,
	})
	return err
}
