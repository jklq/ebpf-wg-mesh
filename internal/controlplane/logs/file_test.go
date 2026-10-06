package logs

import (
	"context"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileLogs(t testing.TB, budget int64) *LogStore {
	t.Helper()
	store, err := OpenLogStore(context.Background(), config.LogCaptureConfig{File: config.FileLogConfig{Directory: t.TempDir(), MaxBytes: budget, SegmentBytes: 256 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func TestFileLogsDurableFilteredPaginationAndPurge(t *testing.T) {
	ctx := context.Background()
	s := fileLogs(t, 1<<20)
	now := time.Now().UTC()
	s.SetProjectResolver(func(context.Context, []string) (map[string]ProjectRetention, error) {
		return map[string]ProjectRetention{"svc": {ProjectID: "p", RetentionDays: 1}}, nil
	})
	var lines []LogLineInput
	for i := 0; i < 6; i++ {
		lines = append(lines, LogLineInput{ID: fmt.Sprint(i), ServiceID: "svc", AllocationID: "a", ObservedAt: now.Add(time.Duration(i) * time.Second), Line: fmt.Sprintf("Hello %d", i)})
	}
	lines = append(lines, LogLineInput{ID: "hidden", ServiceID: "gone", Line: "Hello hidden"})
	if err := s.WriteLogLines(ctx, lines); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteLogLines(ctx, lines); err != nil {
		t.Fatal(err)
	}
	gap := GapInput{ServiceID: "svc", AllocationID: "a", DroppedCount: 1, SummaryID: "summary", WindowStart: now, WindowEnd: now}
	if err := s.WriteGaps(ctx, []GapInput{gap}); err != nil {
		t.Fatal(err)
	}
	gap.DroppedCount = 8
	gap.WindowEnd = now.Add(time.Second)
	if err := s.WriteGaps(ctx, []GapInput{gap}); err != nil {
		t.Fatal(err)
	}
	dir := s.file.cfg.Directory
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLogStore(ctx, config.LogCaptureConfig{File: config.FileLogConfig{Directory: dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	req := &platformv1.ListServiceLogsRequest{ServiceId: "svc", AllocationId: "a", Search: "HELLO", Limit: 2}
	var ids []string
	for {
		page, err := reopened.ListServiceLogs(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Gaps) != 1 || page.Gaps[0].DroppedCount != 8 {
			t.Fatalf("gaps: %+v", page.Gaps)
		}
		for _, l := range page.Lines {
			ids = append(ids, l.LineID)
			if l.ProjectID != "p" {
				t.Fatal(l)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		req.PageToken = page.NextPageToken
	}
	if strings.Join(ids, ",") != "5,4,3,2,1,0" {
		t.Fatal(ids)
	}
	req.PageToken = ""
	req.StartTime = timestamppb.New(now.Add(4 * time.Second))
	page, err := reopened.ListServiceLogs(ctx, req)
	if err != nil || len(page.Lines) != 2 || len(page.Gaps) != 0 {
		t.Fatalf("range %+v %v", page, err)
	}
	if err := reopened.PurgeProjectLogs(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	req.StartTime = nil
	page, err = reopened.ListServiceLogs(ctx, req)
	if err != nil || len(page.Lines)+len(page.Gaps) != 0 {
		t.Fatalf("purge %+v %v", page, err)
	}
}
func TestFileLogsSizeExpiryAndSingleWriter(t *testing.T) {
	ctx := context.Background()
	s := fileLogs(t, 256<<10)
	dir := s.file.cfg.Directory
	if _, err := OpenLogStore(ctx, config.LogCaptureConfig{File: config.FileLogConfig{Directory: dir}}); err == nil {
		t.Fatal("second writer accepted")
	}
	for i := 0; i < 20; i++ {
		if err := s.WriteLogLines(ctx, []LogLineInput{{ID: fmt.Sprint(i), ServiceID: "svc", ProjectID: "p", ObservedAt: time.Now(), Line: strings.Repeat("x", 20000)}}); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := s.file.paths()
	var bytes int64
	for _, p := range paths {
		info, _ := os.Stat(p)
		bytes += info.Size()
	}
	if bytes > 256<<10 {
		t.Fatal(bytes)
	}
	expired := fileRecord{ID: "expired", ExpiresAt: time.Now().Add(-time.Hour), Line: &LogLineInput{ServiceID: "svc", ProjectID: "p"}}
	p := filepath.Join(dir, "000-expired.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(expired); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenLogStore(ctx, config.LogCaptureConfig{File: config.FileLogConfig{Directory: dir, MaxBytes: 256 << 10, SegmentBytes: 256 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("expired segment retained", err)
	}
}
func TestFileLogsRecoverPartialAppendAndContinueSameSegment(t *testing.T) {
	ctx := context.Background()
	s := fileLogs(t, 1<<20)
	dir := s.file.cfg.Directory
	for i := 0; i < 3; i++ {
		if err := s.WriteLogLines(ctx, []LogLineInput{{ID: fmt.Sprint(i), ProjectID: "p", ServiceID: "svc", ObservedAt: time.Now(), Line: "acknowledged"}}); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := s.file.paths()
	if len(paths) != 1 {
		t.Fatalf("small writes should share a segment, got %d", len(paths))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths[0], os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"line":{"id":"unacknowledged`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenLogStore(ctx, config.LogCaptureConfig{File: config.FileLogConfig{Directory: dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.WriteLogLines(ctx, []LogLineInput{{ID: "after-restart", ProjectID: "p", ServiceID: "svc", ObservedAt: time.Now(), Line: "new"}}); err != nil {
		t.Fatal(err)
	}
	page, err := r.ListServiceLogs(ctx, &platformv1.ListServiceLogsRequest{ServiceId: "svc"})
	if err != nil || len(page.Lines) != 4 {
		t.Fatalf("recovery lost acknowledged lines: %+v, %v", page, err)
	}
}

func BenchmarkLogBackend(b *testing.B) {
	for _, backend := range []string{"file", "clickhouse"} {
		b.Run(backend, func(b *testing.B) {
			cfg := config.LogCaptureConfig{File: config.FileLogConfig{Directory: b.TempDir()}}
			if backend == "clickhouse" {
				cfg.ClickHouse.URL = os.Getenv("COMPACT_BENCH_CLICKHOUSE_URL")
				if cfg.ClickHouse.URL == "" {
					b.Skip("set COMPACT_BENCH_CLICKHOUSE_URL")
				}
			}
			s, err := OpenLogStore(context.Background(), cfg)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			lines := make([]LogLineInput, 100)
			for i := range lines {
				lines[i] = LogLineInput{ID: fmt.Sprintf("bench-%s-%d", backend, i), ProjectID: "bench", ServiceID: "bench", ObservedAt: time.Now(), Line: strings.Repeat("x", 200)}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.WriteLogLines(context.Background(), lines); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
