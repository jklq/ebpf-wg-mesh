package controlplane

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
)

// DeletionPreview reports the live dependents a delete would tombstone.
type DeletionPreview struct {
	Environments []PreviewEnvironment
	Services     []PreviewService
	Domains      []PreviewDomain
	Volumes      []PreviewVolume
}

// PreviewEnvironment is one environment in a deletion preview.
type PreviewEnvironment struct {
	ID           string
	Name         string
	IsProduction bool
}

// PreviewService is one service in a deletion preview.
type PreviewService struct {
	ID              string
	Name            string
	EnvironmentID   string
	EnvironmentName string
}

// PreviewDomain is one domain binding in a deletion preview.
type PreviewDomain struct {
	Hostname          string
	ServiceID         string
	ServiceName       string
	PlatformGenerated bool
}

// PreviewVolume is one volume in a deletion preview.
type PreviewVolume struct {
	ID            string
	Name          string
	EnvironmentID string
}

// previewProjectDeletion lists the live environments, services, domains, and
// volumes a project delete would tombstone.
func (s *catalogPersistence) previewProjectDeletion(ctx context.Context, user authz.User, projectID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeProject(ctx, user, projectID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		preview.Environments, err = listPreviewEnvironments(ctx, tx, scope.ID())
		if err != nil {
			return err
		}
		preview.Services, err = listPreviewServices(ctx, tx, `e.project_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Domains, err = listPreviewDomains(ctx, tx, `e.project_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Volumes, err = listPreviewVolumes(ctx, tx, `e.project_id = $1`, scope.ID())
		return err
	})
	return preview, err
}

// previewEnvironmentDeletion lists the live services, domains, and volumes
// an environment delete would tombstone.
func (s *catalogPersistence) previewEnvironmentDeletion(ctx context.Context, user authz.User, environmentID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeEnvironment(ctx, user, environmentID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		preview.Services, err = listPreviewServices(ctx, tx, `s.environment_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Domains, err = listPreviewDomains(ctx, tx, `s.environment_id = $1`, scope.ID())
		if err != nil {
			return err
		}
		preview.Volumes, err = listPreviewVolumes(ctx, tx, `v.environment_id = $1`, scope.ID())
		return err
	})
	return preview, err
}

// previewVolumeDeletion lists the live services referencing a volume and
// their live domains. A non-empty service list means deletion is refused.
func (s *catalogPersistence) previewVolumeDeletion(ctx context.Context, user authz.User, volumeID string) (DeletionPreview, error) {
	scope, err := s.authz.AuthorizeVolume(ctx, user, volumeID, authz.Read)
	if err != nil {
		return DeletionPreview{}, err
	}
	var preview DeletionPreview
	err = s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var volumeName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM volumes WHERE id = $1`, scope.ID()).Scan(&volumeName); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT s.id, s.name, r.spec_json
			   FROM live_services s
			   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
			  WHERE s.environment_id = $1
			  ORDER BY s.name ASC, s.id ASC`,
			scope.EnvironmentID(),
		)
		if err != nil {
			return err
		}
		var serviceIDs []string
		for rows.Next() {
			var id, name string
			var rawSpec []byte
			if err := rows.Scan(&id, &name, &rawSpec); err != nil {
				rows.Close()
				return err
			}
			spec, err := deliverycore.LoadServiceSpec(rawSpec)
			if err != nil {
				rows.Close()
				return err
			}
			if deliverycore.ServiceVolumeName(spec) != volumeName {
				continue
			}
			preview.Services = append(preview.Services, PreviewService{ID: id, Name: name, EnvironmentID: scope.EnvironmentID()})
			serviceIDs = append(serviceIDs, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(serviceIDs) == 0 {
			return nil
		}
		domains, err := listPreviewDomainsForServices(ctx, tx, serviceIDs)
		if err != nil {
			return err
		}
		preview.Domains = domains
		return nil
	})
	return preview, err
}

func listPreviewEnvironments(ctx context.Context, tx *sql.Tx, projectID string) ([]PreviewEnvironment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT e.id, e.name, e.is_production
		   FROM environments e
		   JOIN projects p ON p.id = e.project_id
		  WHERE e.project_id = $1
		    AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY e.is_production DESC, e.name ASC, e.id ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewEnvironment
	for rows.Next() {
		var item PreviewEnvironment
		if err := rows.Scan(&item.ID, &item.Name, &item.IsProduction); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewServices(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewService, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT s.id, s.name, s.environment_id, e.name
		   FROM live_services s
		   JOIN environments e ON e.id = s.environment_id
		  WHERE `+predicate+`
		  ORDER BY e.name ASC, s.name ASC, s.id ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewService
	for rows.Next() {
		var item PreviewService
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID, &item.EnvironmentName); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewDomains(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewDomain, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hostname, d.service_id, s.name, d.platform_generated
		   FROM domain_bindings d
		   JOIN live_services s ON s.id = d.service_id
		   JOIN environments e ON e.id = s.environment_id
		  WHERE `+predicate+`
		    AND d.deleted_at IS NULL
		  ORDER BY d.hostname ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewDomain
	for rows.Next() {
		var item PreviewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewVolumes(ctx context.Context, tx *sql.Tx, predicate, arg string) ([]PreviewVolume, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT v.id, v.name, v.environment_id
		   FROM volumes v
		   JOIN environments e ON e.id = v.environment_id
		   JOIN projects p ON p.id = e.project_id
		  WHERE `+predicate+`
		    AND v.deleted_at IS NULL AND e.deleted_at IS NULL AND p.deleted_at IS NULL
		  ORDER BY e.name ASC, v.name ASC, v.id ASC`,
		arg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewVolume
	for rows.Next() {
		var item PreviewVolume
		if err := rows.Scan(&item.ID, &item.Name, &item.EnvironmentID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func listPreviewDomainsForServices(ctx context.Context, tx *sql.Tx, serviceIDs []string) ([]PreviewDomain, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(serviceIDs))
	args := make([]any, len(serviceIDs))
	for i, id := range serviceIDs {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT d.hostname, d.service_id, s.name, d.platform_generated
		   FROM domain_bindings d
		   JOIN services s ON s.id = d.service_id
		  WHERE d.service_id IN (`+strings.Join(placeholders, ", ")+`)
		    AND d.deleted_at IS NULL
		  ORDER BY d.hostname ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewDomain
	for rows.Next() {
		var item PreviewDomain
		if err := rows.Scan(&item.Hostname, &item.ServiceID, &item.ServiceName, &item.PlatformGenerated); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
