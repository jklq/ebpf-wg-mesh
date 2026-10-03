package delivery

import (
	"context"
	"database/sql"
	"fmt"
)

type DeletionKind uint8

const (
	DeleteProject DeletionKind = iota + 1
	DeleteEnvironment
	DeleteVolume
)

// DeletionTarget is the resource whose delivery state is being withdrawn or collected.
// The caller authorizes and locks the resource in the same product transaction.
type DeletionTarget struct {
	Kind DeletionKind
	ID   string
}

func (target DeletionTarget) servicePredicate() (string, error) {
	if target.ID == "" {
		return "", fmt.Errorf("deletion target ID is required")
	}
	switch target.Kind {
	case DeleteProject:
		return `environment_id IN (SELECT id FROM environments WHERE project_id = $1)`, nil
	case DeleteEnvironment:
		return `environment_id = $1`, nil
	default:
		return "", fmt.Errorf("deletion target %d has no services", target.Kind)
	}
}

func (target DeletionTarget) volumePredicate() (string, error) {
	if target.Kind == DeleteVolume {
		if target.ID == "" {
			return "", fmt.Errorf("deletion target ID is required")
		}
		return `id = $1`, nil
	}
	return target.servicePredicate()
}

// QuiesceDeletionTx stops builds and deployments for a project or environment.
// Assignment removal belongs to the same lifecycle operation and transaction.
func QuiesceDeletionTx(ctx context.Context, tx *sql.Tx, sourceStore SourceStore, target DeletionTarget, actorUserID string) error {
	predicate, err := target.servicePredicate()
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE `+predicate+` ORDER BY id`, target.ID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	scanErr := rows.Err()
	if err := rows.Close(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	store := &persistence{sourceStore: sourceStore}
	for _, id := range ids {
		if err := quiesceServiceTx(ctx, store, tx, id, actorUserID); err != nil {
			return err
		}
	}
	return DropDeletionAssignmentsTx(ctx, tx, target)
}

// DropDeletionAssignmentsTx also reasserts empty placement when a resource is
// restored. Only a later environment release may create new assignments.
func DropDeletionAssignmentsTx(ctx context.Context, tx *sql.Tx, target DeletionTarget) error {
	predicate, err := target.servicePredicate()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM allocation_assignments WHERE service_id IN (SELECT id FROM services WHERE `+predicate+`)`, target.ID)
	return err
}
