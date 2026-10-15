package usage

import (
	"context"
	"database/sql"
	"time"
)

// ProducerMonitor is one producer's alarm-relevant state.
type ProducerMonitor struct {
	ProducerID, AgentID, State string
	SealedThrough              sql.NullTime
	LastReportAt               sql.NullTime
	JournalBytes               int64
	JournalCapacityBytes       int64
	Exhausted                  bool
	GapRecords                 uint64
	UnattributedEgressBytes    uint64
	Rejections                 int64
	ProducerGaps               int64
	HasCurrentAttributions     bool
}

// MonitorSnapshot reads every non-superseded producer plus recent rejection
// and producer-gap counts, for operator alarms.
func (s *Store) MonitorSnapshot(ctx context.Context, since time.Time) ([]ProducerMonitor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.producer_id, p.agent_id, p.state, p.sealed_through, p.last_report_at,
			COALESCE((p.health->>'journalBytes')::bigint, 0),
			COALESCE((p.health->>'journalCapacityBytes')::bigint, 0),
			(p.health ? 'exhaustedSince'),
			COALESCE((p.health->>'gapRecords')::bigint, 0),
			COALESCE((p.health->>'unattributedEgressBytes')::bigint, 0),
			(SELECT count(*) FROM usage_intervals i WHERE i.producer_id = p.producer_id AND i.status = 'rejected' AND i.accepted_at >= $1),
			(SELECT count(*) FROM usage_producer_gaps g WHERE g.producer_id = p.producer_id AND g.recorded_at >= $1),
			EXISTS (SELECT 1 FROM usage_attributions a WHERE a.agent_id = p.agent_id AND a.ended_at IS NULL)
		FROM usage_producers p WHERE p.state <> 'superseded'`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProducerMonitor
	for rows.Next() {
		var m ProducerMonitor
		var gaps, unattributed int64
		if err := rows.Scan(&m.ProducerID, &m.AgentID, &m.State, &m.SealedThrough, &m.LastReportAt,
			&m.JournalBytes, &m.JournalCapacityBytes, &m.Exhausted, &gaps, &unattributed,
			&m.Rejections, &m.ProducerGaps, &m.HasCurrentAttributions); err != nil {
			return nil, err
		}
		m.GapRecords, m.UnattributedEgressBytes = uint64(max(gaps, 0)), uint64(max(unattributed, 0))
		out = append(out, m)
	}
	return out, rows.Err()
}
