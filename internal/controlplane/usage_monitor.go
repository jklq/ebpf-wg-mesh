package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"ebof-wg-mesh/internal/alarm"
)

// usageMonitorLoop alarms once per condition while it holds: journal
// pressure, exhaustion, gaps, stale producers of active agents, blocked
// producers, rejections, and growing unattributed egress.
func (s *Server) usageMonitorLoop(ctx context.Context) {
	alarmed := map[string]uint64{}
	fire := func(key string, marker uint64, event, subject, message, next string) {
		if prev, ok := alarmed[key]; ok && prev >= marker && marker != 0 {
			return
		}
		if _, ok := alarmed[key]; ok && marker == 0 {
			return
		}
		alarmed[key] = marker
		if err := s.alarms.Send(ctx, alarm.Event{Event: event, Subject: subject, Message: message, NextAction: next}); err != nil {
			slog.Warn("send usage alarm", "event", event, "error", err)
		}
	}
	check := func() {
		snapshot, err := s.usage.MonitorSnapshot(ctx, time.Now().Add(-10*time.Minute))
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("usage monitor", "error", err)
			}
			return
		}
		active := map[string]bool{}
		for _, p := range snapshot {
			base := "usage:" + p.ProducerID
			if p.State == "blocked" {
				active[base+":blocked"] = true
				fire(base+":blocked", 0, "usage_producer_blocked", p.AgentID,
					fmt.Sprintf("usage producer %s on agent %s is blocked by a conflicting record", p.ProducerID, p.AgentID),
					"inspect the producer conflict and the agent journal")
			}
			if p.Exhausted || (p.JournalCapacityBytes > 0 && p.JournalBytes*100/p.JournalCapacityBytes > 80) {
				active[base+":pressure"] = true
				fire(base+":pressure", 0, "usage_journal_pressure", p.AgentID,
					fmt.Sprintf("usage journal on agent %s is above 80%% full (%d of %d bytes)", p.AgentID, p.JournalBytes, p.JournalCapacityBytes),
					"restore agent connectivity or raise the journal cap")
			}
			if p.HasCurrentAttributions && p.State == "active" &&
				(!p.SealedThrough.Valid || time.Since(p.SealedThrough.Time) > 5*time.Minute) {
				active[base+":stale"] = true
				fire(base+":stale", 0, "usage_producer_stale", p.AgentID,
					fmt.Sprintf("usage producer on agent %s has not sealed usage recently", p.AgentID),
					"check the agent and its metering health")
			}
			if p.GapRecords > 0 {
				fire(base+":gaps", p.GapRecords, "usage_gaps", p.AgentID,
					fmt.Sprintf("usage producer on agent %s recorded %d gap records", p.AgentID, p.GapRecords), "")
			}
			if p.ProducerGaps > 0 {
				fire(base+":producer-gaps", uint64(p.ProducerGaps), "usage_producer_gaps", p.AgentID,
					fmt.Sprintf("usage producer on agent %s has %d new producer gaps", p.AgentID, p.ProducerGaps), "")
			}
			if p.Rejections > 0 {
				fire(base+":rejections", uint64(p.Rejections), "usage_rejections", p.AgentID,
					fmt.Sprintf("%d usage records from agent %s were rejected recently", p.Rejections, p.AgentID),
					"inspect rejected usage records")
			}
			if p.UnattributedEgressBytes > 0 {
				fire(base+":unattributed", p.UnattributedEgressBytes, "usage_unattributed_egress", p.AgentID,
					fmt.Sprintf("agent %s counted %d output bytes without an epoch counter", p.AgentID, p.UnattributedEgressBytes),
					"check egress counter registration")
			}
		}
		for key := range alarmed {
			if !active[key] && alarmed[key] == 0 {
				delete(alarmed, key)
			}
		}
	}
	check()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
