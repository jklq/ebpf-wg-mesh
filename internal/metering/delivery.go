package metering

import (
	"context"
	"log/slog"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	ackTimeout     = 30 * time.Second
	healthInterval = 60 * time.Second
	deliveryPoll   = 5 * time.Second
)

// Attach binds delivery to a live Sync session's send function.
func (m *Meter) Attach(agentID, sessionID string, send func(*agentv1.AgentClientMessage) error) {
	m.sendMu.Lock()
	m.agentID, m.sessionID, m.send = agentID, sessionID, send
	m.sendMu.Unlock()
	m.wake()
}

// Detach unbinds delivery when the session ends.
func (m *Meter) Detach() {
	m.sendMu.Lock()
	m.send = nil
	m.sendMu.Unlock()
}

// Acknowledge applies a control-plane acknowledgement: advance (or rewind,
// after a restore) the send cursor, and stop on conflict.
func (m *Meter) Acknowledge(ack *agentv1.UsageAcknowledgement) {
	if ack == nil || ack.GetProducerId() != m.ProducerID() {
		return
	}
	m.sendMu.Lock()
	if ack.GetConflict() != "" {
		m.conflict = true
		slog.Error("usage delivery stopped: producer conflict", "conflict", ack.GetConflict())
	}
	m.sendMu.Unlock()
	if err := m.journal.acknowledge(ack.GetAcceptedThrough()); err != nil {
		slog.Warn("apply usage acknowledgement", "error", err)
		return
	}
	m.sendMu.Lock()
	m.sendFrom = ack.GetAcceptedThrough() + 1
	m.sendMu.Unlock()
	m.wake()
}

// Deliver sends journal records while attached, one report in flight, plus
// an empty health report about every minute.
func (m *Meter) Deliver(ctx context.Context) {
	var inFlightSince time.Time
	var inFlight bool
	lastHealth := time.Now()
	ticker := time.NewTicker(deliveryPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.notify:
		case <-ticker.C:
		}
		m.sendMu.Lock()
		send, conflict := m.send, m.conflict
		m.sendMu.Unlock()
		if send == nil || conflict {
			continue
		}
		if inFlight && time.Since(inFlightSince) < ackTimeout {
			continue
		}
		inFlight = false // timed out or acked (ack clears via cursor move below)
		m.sendMu.Lock()
		from := m.sendFrom
		m.sendMu.Unlock()
		records, lostThrough, err := m.journal.batch(from)
		if err != nil {
			slog.Warn("read usage batch", "error", err)
			continue
		}
		if len(records) == 0 && time.Since(lastHealth) < healthInterval {
			continue
		}
		m.sendMu.Lock()
		m.reportSeq++
		report := &agentv1.UsageReport{
			AgentId: m.agentID, SessionId: m.sessionID,
			InstallationId: m.journal.meta.InstallationID, ProducerId: m.journal.meta.ProducerID,
			ReportId: m.reportSeq, Records: records, LostThrough: lostThrough,
			Health: m.health(),
		}
		m.sendMu.Unlock()
		// sealed_through is honest only when this batch reaches the newest
		// sealed record.
		if newest := m.journal.meta.NextSequence - 1; len(records) > 0 &&
			records[len(records)-1].GetSequence() >= newest && m.journal.meta.SealedThroughMS > 0 {
			report.SealedThrough = timestamppb.New(time.UnixMilli(m.journal.meta.SealedThroughMS).UTC())
		} else if len(records) == 0 && m.journal.meta.SealedThroughMS > 0 {
			report.SealedThrough = timestamppb.New(time.UnixMilli(m.journal.meta.SealedThroughMS).UTC())
		}
		if err := send(&agentv1.AgentClientMessage{
			Payload: &agentv1.AgentClientMessage_UsageReport{UsageReport: report}}); err != nil {
			slog.Warn("send usage report", "error", err)
			continue
		}
		lastHealth = time.Now()
		if len(records) > 0 {
			inFlight, inFlightSince = true, time.Now()
		}
	}
}

func (m *Meter) health() *agentv1.UsageProducerHealth {
	// Estimate the record byte rate over a rolling window for offline
	// capacity reporting.
	now := time.Now()
	m.mu.Lock()
	if m.rateWindowStart.IsZero() {
		m.rateWindowStart, m.rateWindowBytes = now, m.journal.meta.RecordBytes
	} else if elapsed := now.Sub(m.rateWindowStart).Seconds(); elapsed >= 60 {
		delta := m.journal.meta.RecordBytes - m.rateWindowBytes
		if delta >= 0 {
			m.rateBytes = float64(delta) / elapsed
		}
		m.rateWindowStart, m.rateWindowBytes = now, m.journal.meta.RecordBytes
	}
	rate := m.rateBytes
	m.mu.Unlock()
	h := m.journal.health(rate)
	h.HostCpus, h.HostMemoryBytes = m.cfg.HostCPUs, m.cfg.HostMemoryBytes
	return h
}

// Health snapshots producer health for tests and alarms.
func (m *Meter) Health() *agentv1.UsageProducerHealth { return m.health() }
