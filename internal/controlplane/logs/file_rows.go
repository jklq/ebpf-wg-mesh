package logs

import (
	"context"
	"ebof-wg-mesh/internal/logpipeline"
	"strings"
	"time"
)

func (s *LogStore) writeFileLines(ctx context.Context, inputs []LogLineInput) error {
	resolved, err := s.resolveProjects(ctx, inputs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	records := make([]fileRecord, 0, len(inputs))
	for _, in := range inputs {
		if strings.TrimSpace(in.ServiceID) == "" {
			continue
		}
		project, expiry, ok := s.attribution(in.ProjectID, in.ExpiresAt, in.ServiceID, resolved, now)
		if !ok {
			continue
		}
		in.ProjectID = project
		in.ExpiresAt = expiry
		if in.ObservedAt.IsZero() {
			in.ObservedAt = now
		}
		in.ObservedAt = in.ObservedAt.UTC()
		if strings.TrimSpace(in.ID) == "" {
			in.ID = logpipeline.SyntheticLineID()
		}
		var truncated bool
		in.Line, truncated = logpipeline.TruncateLine(in.Line)
		in.Truncated = in.Truncated || truncated
		in.Attributes = logpipeline.NormalizeAttributes(in.Attributes)
		in.Stream = normalizeLogStream(in.Stream)
		in.LogType = normalizeLogType(in.LogType)
		in.Stage = normalizeStageName(in.Stage)
		in.Event = logpipeline.NormalizeEvent(in.Event)
		records = append(records, fileRecord{Line: &in, ID: in.ID, ExpiresAt: expiry})
	}
	return s.file.append(ctx, records)
}
func (s *LogStore) writeFileGaps(ctx context.Context, gaps []GapInput) error {
	inputs := make([]LogLineInput, 0, len(gaps))
	for _, g := range gaps {
		inputs = append(inputs, LogLineInput{ServiceID: g.ServiceID, ProjectID: g.ProjectID, ExpiresAt: g.ExpiresAt})
	}
	resolved, err := s.resolveProjects(ctx, inputs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	records := make([]fileRecord, 0, len(gaps))
	for _, g := range gaps {
		if g.ServiceID == "" || g.DroppedCount == 0 {
			continue
		}
		project, expiry, ok := s.attribution(g.ProjectID, g.ExpiresAt, g.ServiceID, resolved, now)
		if !ok {
			continue
		}
		g.ProjectID = project
		g.ExpiresAt = expiry
		if g.WindowStart.IsZero() {
			g.WindowStart = now
		}
		if g.WindowEnd.IsZero() || g.WindowEnd.Before(g.WindowStart) {
			g.WindowEnd = g.WindowStart
		}
		g.LogType = normalizeLogType(g.LogType)
		id := gapIdentity(g.ServiceID, g.AllocationID, g.BuildID, string(g.LogType), g.Stream, g.Reason, g.Reporter, g.SummaryID, g.WindowStart, g.WindowEnd, g.DroppedCount)
		g.Stream = normalizeLogStream(g.Stream)
		g.Reason = logpipeline.NormalizeDropReason(g.Reason)
		records = append(records, fileRecord{Gap: &g, ID: id, ExpiresAt: expiry})
	}
	return s.file.append(ctx, records)
}
