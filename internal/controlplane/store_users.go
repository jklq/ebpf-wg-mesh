package controlplane

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) ensurePrincipal(ctx context.Context, subject, email string) (principalRecord, error) {
	var principal principalRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO users(subject, email, created_at)
			 VALUES ($1, $2, $3)
			 ON CONFLICT(subject) DO UPDATE SET email = excluded.email`,
			subject,
			email,
			now,
		); err != nil {
			return err
		}
		var err error
		principal, err = s.principalBySubjectQuerier(ctx, tx, subject)
		return err
	})
	if err != nil {
		return principalRecord{}, err
	}
	return principal, nil
}

func (s *Store) principalBySubjectQuerier(ctx context.Context, q serviceQueryer, subject string) (principalRecord, error) {
	var rec principalRecord
	err := q.QueryRowContext(
		ctx,
		`SELECT subject, email, created_at FROM users WHERE subject = $1`,
		subject,
	).Scan(&rec.Subject, &rec.Email, &rec.CreatedAt)
	if err != nil {
		return principalRecord{}, err
	}
	return rec, nil
}
