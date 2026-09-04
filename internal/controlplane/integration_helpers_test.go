//go:build integration

package controlplane

import (
	"context"
	"database/sql"
)

func enqueueBuildForTest(ctx context.Context, store *Store, userID, serviceID, commitSHA string) (buildRunRecord, error) {
	var build buildRunRecord
	err := store.withTx(ctx, func(tx *sql.Tx) error {
		service, err := store.serviceByIDQuerier(ctx, tx, userID, serviceID)
		if err != nil {
			return err
		}
		build, err = store.enqueueBuildTx(
			ctx,
			tx,
			service,
			commitSHA,
			deploymentActor{Kind: deploymentCauseUser, ID: userID},
		)
		return err
	})
	return build, err
}

func releaseEnvironmentServiceForTest(ctx context.Context, store *Store, userID, environmentID, serviceID string) (serviceRecord, error) {
	services, _, err := store.releaseEnvironment(ctx, userID, environmentID)
	if err != nil {
		return serviceRecord{}, err
	}
	for _, service := range services {
		if service.ID == serviceID {
			return service, nil
		}
	}
	return serviceRecord{}, sql.ErrNoRows
}
