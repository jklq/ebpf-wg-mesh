package logs

import (
	"context"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/logpipeline"
	"sort"
	"strings"
	"time"
)

// boundedPage keeps the newest limit+1 distinct ordering keys while scanning.
type filePageItem struct {
	record fileRecord
	at     time.Time
	key    string
}

func addFilePage(page []filePageItem, item filePageItem, limit int) []filePageItem {
	i := sort.Search(len(page), func(i int) bool {
		return page[i].at.Before(item.at) || page[i].at.Equal(item.at) && page[i].key <= item.key
	})
	if i < len(page) && page[i].at.Equal(item.at) && page[i].key == item.key {
		page[i] = item
		return page
	}
	if i >= limit {
		return page
	}
	page = append(page, filePageItem{})
	copy(page[i+1:], page[i:])
	page[i] = item
	if len(page) > limit {
		page = page[:limit]
	}
	return page
}
func (f *fileStore) list(ctx context.Context, req *platformv1.ListServiceLogsRequest) (ServiceLogPage, error) {
	var result ServiceLogPage
	cursor, id, err := logpipeline.DecodeCursor(req.GetPageToken())
	if err != nil {
		return result, err
	}
	gapCursor, gapID, err := logpipeline.DecodeCursor(req.GetGapPageToken())
	if err != nil {
		return result, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultLogQueryLimit
	}
	limit = min(limit, maxLogQueryLimit)
	f.mu.Lock()
	defer f.mu.Unlock()
	paths, err := f.paths()
	if err != nil {
		return result, err
	}
	var lines, gaps []filePageItem
	now := time.Now()
	search := strings.ToLower(strings.TrimSpace(req.GetSearch()))
	kind := logTypeFromProto(req.GetLogType())
	matches := func(service, allocation, build string, t LogType, isGap bool) bool {
		return service == req.GetServiceId() && (req.GetAllocationId() == "" || allocation == req.GetAllocationId() || isGap && allocation == "") && (req.GetBuildId() == "" || build == req.GetBuildId() || isGap && build == "") && (kind == "" || kind == t)
	}
	older := func(at time.Time, key string, cursor time.Time, id string) bool {
		return cursor.IsZero() || at.Before(cursor) || at.Equal(cursor) && key < id
	}
	for _, path := range paths {
		err = scanFile(ctx, path, func(r fileRecord) error {
			if !r.ExpiresAt.After(now) {
				return nil
			}
			if l := r.Line; l != nil {
				if !matches(l.ServiceID, l.AllocationID, l.BuildID, l.LogType, false) || !older(l.ObservedAt, r.ID, cursor, id) || search != "" && !strings.Contains(strings.ToLower(l.Line), search) {
					return nil
				}
				if req.GetStartTime() != nil && l.ObservedAt.Before(req.GetStartTime().AsTime()) || req.GetEndTime() != nil && l.ObservedAt.After(req.GetEndTime().AsTime()) {
					return nil
				}
				lines = addFilePage(lines, filePageItem{r, l.ObservedAt, r.ID}, limit+1)
			}
			if g := r.Gap; g != nil {
				if !matches(g.ServiceID, g.AllocationID, g.BuildID, g.LogType, true) || !older(g.WindowStart, r.ID, gapCursor, gapID) {
					return nil
				}
				if req.GetStartTime() != nil && g.WindowEnd.Before(req.GetStartTime().AsTime()) || req.GetEndTime() != nil && g.WindowStart.After(req.GetEndTime().AsTime()) {
					return nil
				}
				gaps = addFilePage(gaps, filePageItem{r, g.WindowStart, r.ID}, maxLogGapResults+1)
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	if len(lines) > limit {
		last := lines[limit-1]
		result.NextPageToken = logpipeline.EncodeCursor(last.at, last.key)
		lines = lines[:limit]
	}
	for _, item := range lines {
		l := item.record.Line
		result.Lines = append(result.Lines, ServiceLog{ObservedAt: l.ObservedAt, ProjectID: l.ProjectID, EnvironmentID: l.EnvironmentID, ServiceID: l.ServiceID, AllocationID: l.AllocationID, AgentID: l.AgentID, Stream: l.Stream, RolloutGeneration: l.RolloutGeneration, Sequence: l.Sequence, LineID: l.ID, Line: l.Line, LogType: string(l.LogType), BuildID: l.BuildID, Stage: l.Stage, Event: l.Event, Attributes: l.Attributes, Truncated: l.Truncated})
	}
	if len(gaps) > maxLogGapResults {
		last := gaps[maxLogGapResults-1]
		result.NextGapPageToken = logpipeline.EncodeCursor(last.at, last.key)
		gaps = gaps[:maxLogGapResults]
	}
	for _, item := range gaps {
		g := item.record.Gap
		result.Gaps = append(result.Gaps, ServiceLogGap{AllocationID: g.AllocationID, BuildID: g.BuildID, LogType: string(g.LogType), Stream: g.Stream, DroppedCount: g.DroppedCount, Reason: g.Reason, WindowStart: g.WindowStart, WindowEnd: g.WindowEnd})
	}
	return result, nil
}
