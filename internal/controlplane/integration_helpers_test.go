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
	services, _, err := releaseEnvironmentForTest(ctx, store, userID, environmentID)
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

// Release fixtures use the same authorized operation as the transport.
func releaseEnvironmentForTest(ctx context.Context, store *Store, userID, environmentID string) ([]serviceRecord, []string, error) {
	notifier := &releaseTestNotifier{}
	released, err := NewDelivery(store, notifier).ReleaseEnvironment(context.WithValue(ctx, delegatedUserContextKey{}, DelegatedUser{UserID: userID}), environmentID)
	services := make([]serviceRecord, 0, len(released))
	for _, result := range released {
		services = append(services, result.Service)
	}
	return services, notifier.agentIDs, err
}

type releaseTestNotifier struct{ agentIDs []string }

func (n *releaseTestNotifier) Notify(id string) { n.agentIDs = append(n.agentIDs, id) }
