// Package dbtx contains transaction infrastructure shared by control-plane modules.
package dbtx

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func BumpDesiredRevisions(ctx context.Context, tx *sql.Tx, agentIDs []string) error {
	if len(agentIDs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(agentIDs))
	uniqueIDs := make([]string, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		uniqueIDs = append(uniqueIDs, agentID)
	}
	if len(uniqueIDs) == 0 {
		return nil
	}
	sort.Strings(uniqueIDs)

	args := make([]any, 0, len(uniqueIDs))
	placeholders := make([]string, 0, len(uniqueIDs))
	for i, agentID := range uniqueIDs {
		args = append(args, agentID)
		var b strings.Builder
		b.Grow(len(strconv.Itoa(i+1)) + 1)
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(i + 1))
		placeholders = append(placeholders, b.String())
	}
	query := fmt.Sprintf(
		`UPDATE agents SET desired_revision = desired_revision + 1 WHERE id IN (%s)`,
		strings.Join(placeholders, ", "),
	)
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func BumpAllDesiredRevisions(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE agents SET desired_revision = desired_revision + 1`)
	return err
}
