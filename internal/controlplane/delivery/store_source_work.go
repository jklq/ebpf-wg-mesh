package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/source"
)

func (s *persistence) loadServiceSourceSummaryQuerier(ctx context.Context, q ServiceQueryer, spec *platformv1.ServiceSpec, serviceID string) (*platformv1.ServiceSourceSummary, error) {
	if spec == nil || spec.GetSource() == nil {
		return nil, nil
	}
	if image := spec.GetSource().GetImage(); image != nil {
		return BuildSourceSummary(spec), nil
	}
	desired := source.DesiredSourceSpec(spec)
	if desired == nil {
		return nil, nil
	}
	binding, err := s.sourceStore.SourceBindingByServiceIDQuerier(ctx, q, serviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, nil, nil, nil), nil
	case err != nil:
		return nil, err
	}
	revision, err := s.sourceStore.LatestSourceRevisionByBindingIDTx(ctx, q, binding.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return toProtoSourceStateSummary(desired, &binding, nil, nil), nil
	case err != nil:
		return nil, err
	}
	snapshot, err := s.sourceStore.SourceSnapshotByRevisionIDTx(ctx, q, revision.ID)
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
	key := fmt.Sprintf("%s:%s:%d", source.SourceWorkKindSourceSpecChanged, serviceID, specRevision)
	if force {
		key = fmt.Sprintf("%s:%s:%d:%d", source.SourceWorkKindSourceSpecChanged, serviceID, specRevision, time.Now().UTC().UnixNano())
	}
	_, err := s.enqueueSourceWorkItemTx(ctx, tx, source.SourceWorkItemRecord{
		Kind:           source.SourceWorkKindSourceSpecChanged,
		IdempotencyKey: key,
		ServiceID:      serviceID,
		SpecRevision:   specRevision,
	})
	return err
}
