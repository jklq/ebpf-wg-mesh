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
			name := strings.TrimSpace(bootstrap.Name)
			if name == "" {
				name = agentID
			}
			region := strings.TrimSpace(bootstrap.Region)
			if region == "" {
				region = "default"
			}
			failureDomain := strings.TrimSpace(bootstrap.FailureDomain)
			if failureDomain == "" {
				failureDomain = strings.ToLower(agentID)
				failureDomain = strings.Map(func(r rune) rune {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
						return r
					}
					return '-'
				}, failureDomain)
			}
			if err := validateFleetAgentInput(agentID, name, region, bootstrap.Zone, failureDomain, bootstrap.ReservedCPUMillis, bootstrap.ReservedMemoryMebibytes); err != nil {
				return fmt.Errorf("configured agent %s: %w", agentID, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agents(
				id, name, lifecycle_state, region, zone, failure_domain,
				reserved_cpu_millis, reserved_memory_mebibytes, last_seen_at, created_at, updated_at
			) VALUES ($1, $2, 'enrolling', $3, $4, $5, $6, $7, $8, $9, $9)
			ON CONFLICT(id) DO NOTHING`, agentID, name, region, strings.TrimSpace(bootstrap.Zone), failureDomain,
				bootstrap.ReservedCPUMillis, bootstrap.ReservedMemoryMebibytes, time.Unix(0, 0).UTC(), now); err != nil {
				return fmt.Errorf("store configured fleet agent %s: %w", agentID, err)
			}
			hash := bootstrapTokenHash(token)
			configured[string(hash[:])] = struct{}{}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO agent_bootstrap_tokens(token_hash, agent_id, origin, created_at, consumed_at)
				 VALUES ($1, $2, 'config', $3, NULL)
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
			`SELECT token_hash FROM agent_bootstrap_tokens WHERE consumed_at IS NULL AND origin = 'config'`,
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
