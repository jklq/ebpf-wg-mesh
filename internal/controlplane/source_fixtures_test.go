//go:build integration

package controlplane

import (
	"context"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func sourceSummaryForTest(ctx context.Context, s *persistence, id string) (*platformv1.ServiceSourceSummary, error) {
	service, err := s.reads.ServiceSnapshot(ctx, id)
	return service.SourceSummary, err
}
