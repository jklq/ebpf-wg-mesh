package signkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
)

// ResetForRecovery atomically replaces every authority key with no overlap.
// The generation marker makes repetition after an interrupted host-admin call
// preserve the same CA and identities. This method is not exposed through RPC.
func (s *Service) ResetForRecovery(ctx context.Context, installation, generation string) error {
	if installation == "" || generation == "" {
		return fmt.Errorf("recovery requires installation and generation")
	}
	materials := map[string]*generatedMaterial{}
	for _, scope := range AllScopes() {
		mat, err := s.generate(ctx, scope, RotateOptions{})
		if err != nil {
			return err
		}
		materials[scope] = mat
	}
	return sqlretry.ExecuteTx(ctx, s.db, nil, func(tx *sql.Tx) error {
		var oldInstallation, oldGeneration string
		err := tx.QueryRowContext(ctx, `SELECT installation,generation FROM recovery_runtime_authority WHERE singleton = TRUE FOR UPDATE`).Scan(&oldInstallation, &oldGeneration)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if oldInstallation != "" && oldInstallation != installation {
			return fmt.Errorf("recovery cannot change installation identity")
		}
		if oldGeneration == generation {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM platform_signing_keys`); err != nil {
			return err
		}
		for scope, mat := range materials {
			if _, err := tx.ExecContext(ctx, `INSERT INTO platform_signing_keys(id,scope,kid,state,key_type,wrapping_key_id,wrapped_key,public_pem,created_at,updated_at)
				VALUES ($1,$2,$3,'active',$4,$5,$6,$7,$8,$8)`, mat.id, scope, mat.kid, mat.keyType, mat.wrappingKeyID, mat.wrapped, string(mat.publicPEM), time.Now().UTC()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_certificates`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_bootstrap_tokens`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM control_plane_leases`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_authority SET epoch = 1, outstanding_not_after = statement_timestamp() WHERE id = 1`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPSERT INTO recovery_runtime_authority(singleton,installation,generation,paused) VALUES (TRUE,$1,$2,TRUE)`, installation, generation)
		return err
	})
}
