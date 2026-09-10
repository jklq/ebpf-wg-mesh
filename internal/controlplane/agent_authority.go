package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type agentSessions interface {
	Serving() bool
	Grant(string, string, uint64, int64) error
	Acknowledge(string, string, uint64, int64) error
}

func (s *fleetPersistence) grantAgentCommand(ctx context.Context, agentID, sessionID string, epoch uint64, cursor int64) (time.Time, error) {
	if s.sessions == nil {
		return time.Time{}, deliverycore.ErrNotLiveOwner
	}
	if err := s.sessions.Grant(agentID, sessionID, epoch, cursor); err != nil {
		if errors.Is(err, deliverycore.ErrStaleAgentSession) {
			return time.Time{}, errors.New("agent session superseded")
		}
		return time.Time{}, err
	}
	var deadline time.Time
	err := s.withTxUnfenced(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1 FOR UPDATE`).Scan(&current); err != nil {
			return err
		}
		if uint64(current) != epoch {
			return errors.New("authority epoch changed; reconnect required")
		}
		if cursor < 0 {
			return errors.New("invalid desired-state cursor")
		}
		now, err := dbtx.DatabaseTime(ctx, tx)
		if err != nil {
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

func (s *database) advanceAgentAuthority(ctx context.Context, expected uint64) error {
	return s.withTxUnfenced(ctx, func(ctx context.Context, tx *sql.Tx) error {
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

func (s *fleetPersistence) acknowledgeAgentDesired(ctx context.Context, ack *agentv1.DesiredStateAcknowledgement) error {
	var epoch uint64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM agent_authority WHERE id = 1`).Scan(&epoch); err != nil {
		return err
	}
	if ack.GetAuthorityEpoch() != epoch {
		return fmt.Errorf("stale desired-state acknowledgement")
	}
	return s.sessions.Acknowledge(ack.GetAgentId(), ack.GetSessionId(), ack.GetAuthorityEpoch(), ack.GetReconciliationCursor())
}

func stampAgentCommand(state *agentv1.DesiredNodeState, sessionID string, epoch uint64, deadline time.Time) {
	state.SessionId = sessionID
	state.AuthorityEpoch = epoch
	state.AuthorityNotAfter = timestamppb.New(deadline)
}
