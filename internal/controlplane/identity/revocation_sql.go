package identity

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ebof-wg-mesh/internal/sqlretry"
)

// NewSharedCertificateRevocations binds verification and revocation to the
// shared database. A database outage fails authentication closed on every replica.
func NewSharedCertificateRevocations(db *sql.DB) *CertificateRevocations {
	return &CertificateRevocations{db: db}
}

func (r *CertificateRevocations) addShared(values []string) error {
	serials := make([]string, len(values))
	for n, value := range values {
		serial, err := parseCertificateSerial(value)
		if err != nil {
			return err
		}
		serials[n] = serial
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sqlretry.ExecuteTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		for _, serial := range serials {
			if _, err := tx.ExecContext(ctx, `INSERT INTO certificate_revocations(serial, revoked_at) VALUES ($1, statement_timestamp()) ON CONFLICT(serial) DO NOTHING`, serial); err != nil {
				return fmt.Errorf("record shared certificate revocation: %w", err)
			}
		}
		return nil
	})
}

func (r *CertificateRevocations) checkShared(serial string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var revoked bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM certificate_revocations WHERE serial=$1)`, serial).Scan(&revoked); err != nil {
		return fmt.Errorf("read shared certificate revocations: %w", err)
	}
	if revoked {
		return ErrClientCertificateRevoked
	}
	return nil
}
