package controlplane

import "context"

func (s *readsPersistence) countServiceRevisionsForTest(ctx context.Context, serviceID string) (int, error) {
	var revisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revisions); err != nil {
		return 0, err
	}
	return revisions, nil
}

func (s *readsPersistence) countServiceRolloutsForTest(ctx context.Context, serviceID string) (int, error) {
	var rollouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_rollouts WHERE service_id = $1`, serviceID).Scan(&rollouts); err != nil {
		return 0, err
	}
	return rollouts, nil
}
