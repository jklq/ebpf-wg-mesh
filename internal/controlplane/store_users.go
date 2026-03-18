package controlplane

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) ensurePrincipal(ctx context.Context, subject, email string) (userRecord, error) {
	var user userRecord
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
		user, err = s.userBySubjectQuerier(ctx, tx, subject)
		return err
	})
	if err != nil {
		return userRecord{}, err
	}
	return user, nil
}

func (s *Store) userBySubjectQuerier(ctx context.Context, q serviceQueryer, subject string) (userRecord, error) {
	var rec userRecord
	err := q.QueryRowContext(
		ctx,
		`SELECT subject, email, created_at FROM users WHERE subject = $1`,
		subject,
	).Scan(&rec.Subject, &rec.Email, &rec.CreatedAt)
	if err != nil {
		return userRecord{}, err
	}
	return rec, nil
}
