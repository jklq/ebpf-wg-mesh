package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// grantAgentCommand checks the current session and authority transactionally.
// A paused sender cannot extend a grant after authority has changed. The
// persisted maximum expiry survives sender death, disconnect and early release.
func (s *database) grantAgentCommand(ctx context.Context, agentID, sessionID string, epoch uint64, cursor int64) (time.Time, error) {
	var deadline time.Time
	err := s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&current); err != nil {
			return err
		}
		if uint64(current) != epoch {
			return errors.New("authority epoch changed; reconnect required")
		}
		var session string
		var reachable bool
		if err := tx.QueryRowContext(ctx, `SELECT session_id, reachable FROM agent_presence WHERE agent_id = $1 FOR UPDATE`, agentID).Scan(&session, &reachable); err != nil {
			return err
		}
		if session != sessionID || !reachable {
			return errors.New("agent session superseded")
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if cursor < 0 {
			return errors.New("invalid desired-state cursor")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_presence SET offered_authority_epoch = $3, offered_cursor = $4 WHERE agent_id = $1 AND session_id = $2`, agentID, sessionID, int64(epoch), cursor); err != nil {
			return err
		}
		deadline = now.Add(reconciliation.GrantLifetime)
		_, err = tx.ExecContext(ctx, `UPDATE agent_authority SET outstanding_not_after = greatest(outstanding_not_after, $1) WHERE id = 1`, deadline)
		return err
	})
	return deadline, err
}

func (s *database) agentAuthorityEpoch(ctx context.Context) (uint64, error) {
	var epoch uint64
	err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch)
	return epoch, err
}

// advanceAgentAuthority is the cutover gate. Callers must stop grant issuance
// and retry after the outstanding deadline; disconnecting a holder is not a
// release of its grants. There is deliberately no automatic cutover on reconnect.
func (s *database) advanceAgentAuthority(ctx context.Context, expected uint64) error {
	return s.withTxUnfenced(ctx, func(tx *sql.Tx) error {
		var epoch uint64
		var deadline time.Time
		if err := tx.QueryRowContext(ctx, `SELECT epoch, outstanding_not_after FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&epoch, &deadline); err != nil {
			return err
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if epoch != expected || !reconciliation.CanTakeOver(now, deadline) {
			return errors.New("authority cutover fenced by epoch or outstanding grants")
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_authority SET epoch = epoch + 1 WHERE id = 1`)
		return err
	})
}

func (s *database) acknowledgeAgentDesired(ctx context.Context, ack *agentv1.DesiredStateAcknowledgement) error {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_presence SET accepted_authority_epoch = $3, accepted_cursor = $4
 WHERE agent_id = $1 AND session_id = $2
 AND $4 >= 0 AND offered_authority_epoch = $3 AND offered_cursor >= $4
 AND $3 = (SELECT epoch FROM agent_authority WHERE id = 1)
 AND (accepted_authority_epoch < $3 OR (accepted_authority_epoch = $3 AND accepted_cursor <= $4))`,
		ack.GetAgentId(), ack.GetSessionId(), int64(ack.GetAuthorityEpoch()), ack.GetReconciliationCursor())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	return nil
}

func stampAgentCommand(state *agentv1.DesiredNodeState, sessionID string, epoch uint64, deadline time.Time) {
	state.SessionId = sessionID
	state.AuthorityEpoch = epoch
	state.AuthorityNotAfter = timestamppb.New(deadline)
}
