package controlplane

import (
	"context"
	"database/sql"
	"ebof-wg-mesh/internal/controlplane/dbtx"

	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"time"
)

func (s *catalogPersistence) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, error) {
	if err := deliverycore.ValidatePort(targetPort); err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	var binding deliverycore.DomainBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.reads.deliveryQueries().ProjectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var agentID, environmentID string
		if err := tx.QueryRowContext(ctx, `SELECT a.agent_id, s.environment_id FROM allocations a JOIN services s ON s.id = a.service_id
			JOIN environments e ON e.id = s.environment_id WHERE s.id = $1 AND e.project_id = $2`, serviceID, projectID).Scan(&agentID, &environmentID); err != nil {
			return err
		}

		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx,
			`SELECT d.hostname, e.project_id, s.environment_id, d.service_id, d.target_port, d.created_at, d.updated_at
			   FROM domain_bindings d
			   JOIN services s ON s.id = d.service_id
			   JOIN environments e ON e.id = s.environment_id
			  WHERE d.hostname = $1`,
			hostname,
		).Scan(&binding.Hostname, &binding.ProjectID, &binding.EnvironmentID, &binding.ServiceID, &binding.TargetPort, &binding.CreatedAt, &binding.UpdatedAt)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, service_id, target_port, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, serviceID, targetPort, now, now,
			); err != nil {
				return err
			}
			if err := dbtx.BumpDesiredRevisions(ctx, tx, []string{agentID}); err != nil {
				return err
			}
			binding = deliverycore.DomainBindingRecord{
				Hostname:      hostname,
				ProjectID:     projectID,
				EnvironmentID: environmentID,
				ServiceID:     serviceID,
				TargetPort:    targetPort,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			return nil
		case err != nil:
			return err
		case binding.ProjectID != projectID:
			return deliverycore.ErrDomainAlreadyExists
		case binding.ServiceID == serviceID && binding.TargetPort == targetPort:
			return nil
		default:
			agentIDs := []string{agentID}
			if binding.ServiceID != serviceID {
				previousIDs, err := s.reads.agentIDsForServiceQuerier(ctx, tx, binding.ServiceID)
				if err != nil {
					return err
				}
				agentIDs = append(agentIDs, previousIDs...)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        target_port = $2,
				        updated_at = $3
				  WHERE hostname = $4`,
				serviceID, targetPort, now, hostname,
			); err != nil {
				return err
			}
			if err := dbtx.BumpDesiredRevisions(ctx, tx, agentIDs); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.TargetPort = targetPort
			binding.UpdatedAt = now
			return nil
		}
	})
	if err != nil {
		return deliverycore.DomainBindingRecord{}, err
	}
	return binding, nil
}

func (s *catalogPersistence) createVolumeTx(ctx context.Context, tx *sql.Tx, userID, environmentID, name string, sizeBytes int64) (deliverycore.VolumeRecord, error) {
	environment, err := s.reads.deliveryQueries().EnvironmentByIDQuerier(ctx, tx, userID, environmentID)
	if err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	rec := deliverycore.VolumeRecord{
		ID:            deliverycore.MustID(),
		EnvironmentID: environment.ID,
		Name:          name,
		SizeBytes:     sizeBytes,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, environment_id, name, size_bytes, created_at) VALUES ($1, $2, $3, $4, $5)`,
		rec.ID, rec.EnvironmentID, rec.Name, rec.SizeBytes, rec.CreatedAt,
	); err != nil {
		return deliverycore.VolumeRecord{}, err
	}
	return rec, nil
}
