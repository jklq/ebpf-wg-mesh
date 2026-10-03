package delivery

import (
	"context"
	"database/sql"

	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/controlplane/journal"
)

// EnvironmentSnapshot keeps services, retained volumes, and their invalidation
// index in one committed read. Runtime observations are added separately.
type EnvironmentSnapshot struct {
	Services []ServiceRecord
	Volumes  []VolumeRecord
	Index    int64
}

func (d *Delivery) ReadEnvironmentSnapshot(ctx context.Context, user authz.User, environmentID string, includeDeleted bool) (EnvironmentSnapshot, error) {
	scope, err := d.store.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return EnvironmentSnapshot{}, err
	}
	var snapshot EnvironmentSnapshot
	err = d.store.readState(ctx, func(tx *sql.Tx, _ *journal.Projection) error {
		var err error
		snapshot.Index, err = dbtx.EnvironmentRevision(ctx, tx)
		if err != nil {
			return err
		}
		snapshot.Services, err = d.store.listServicesQuerier(ctx, tx, scope, includeDeleted)
		if err != nil {
			return err
		}
		snapshot.Volumes, err = d.store.listVolumesQuerier(ctx, tx, scope, includeDeleted)
		return err
	})
	return snapshot, err
}
