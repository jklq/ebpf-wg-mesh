package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
)

var errInvalidBootstrapToken = errors.New("invalid, consumed, or incorrectly bound bootstrap token")

func (s *Store) ensureAgentBootstrapTokens(ctx context.Context, tokens []config.AgentBootstrapToken) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		configured := make(map[string]struct{}, len(tokens))
		for _, bootstrap := range tokens {
			agentID := strings.TrimSpace(bootstrap.AgentID)
			token := strings.TrimSpace(bootstrap.Token)
			hash := bootstrapTokenHash(token)
			configured[string(hash[:])] = struct{}{}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO agent_bootstrap_tokens(token_hash, agent_id, created_at, consumed_at)
				 VALUES ($1, $2, $3, NULL)
				 ON CONFLICT(token_hash) DO NOTHING`,
				hash[:], agentID, now,
			); err != nil {
				return fmt.Errorf("store bootstrap token for agent %s: %w", agentID, err)
			}
			var persistedAgentID string
			if err := tx.QueryRowContext(ctx,
				`SELECT agent_id FROM agent_bootstrap_tokens WHERE token_hash = $1`,
				hash[:],
			).Scan(&persistedAgentID); err != nil {
				return fmt.Errorf("load bootstrap token binding: %w", err)
			}
			if persistedAgentID != agentID {
				return fmt.Errorf("bootstrap token is already bound to agent %s", persistedAgentID)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT token_hash FROM agent_bootstrap_tokens WHERE consumed_at IS NULL`,
		)
		if err != nil {
			return fmt.Errorf("list active bootstrap tokens: %w", err)
		}
		var removed [][]byte
		for rows.Next() {
			var hash []byte
			if err := rows.Scan(&hash); err != nil {
				rows.Close()
				return fmt.Errorf("scan active bootstrap token: %w", err)
			}
			if _, ok := configured[string(hash)]; !ok {
				removed = append(removed, append([]byte(nil), hash...))
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close active bootstrap tokens: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate active bootstrap tokens: %w", err)
		}
		for _, hash := range removed {
			if _, err := tx.ExecContext(ctx,
				`UPDATE agent_bootstrap_tokens SET consumed_at = $1 WHERE token_hash = $2 AND consumed_at IS NULL`,
				now, hash,
			); err != nil {
				return fmt.Errorf("revoke removed bootstrap token: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) consumeAgentBootstrapToken(ctx context.Context, agentID, token string) error {
	agentID = strings.TrimSpace(agentID)
	token = strings.TrimSpace(token)
	if agentID == "" || token == "" {
		return errInvalidBootstrapToken
	}
	hash := bootstrapTokenHash(token)
	result, err := s.db.ExecContext(ctx,
		`UPDATE agent_bootstrap_tokens
		    SET consumed_at = $1
		  WHERE token_hash = $2
		    AND agent_id = $3
		    AND consumed_at IS NULL`,
		time.Now().UTC(), hash[:], agentID,
	)
	if err != nil {
		return fmt.Errorf("consume agent bootstrap token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("bootstrap token rows affected: %w", err)
	}
	if rows != 1 {
		return errInvalidBootstrapToken
	}
	return nil
}

func bootstrapTokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.TrimSpace(token)))
}
