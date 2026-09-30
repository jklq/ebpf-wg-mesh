package logpipeline

import (
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

func TestDurableDropsCoalesceWithoutChangingAttribution(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	base := time.Now().UTC()
	key := DropKey{ServiceID: "svc", AllocationID: "a", Reason: ReasonRateLimited}
	if err := s.AddDrops(Drop{Key: key, Count: 3, Start: base, End: base}, Drop{Key: key, Count: 4, Start: base.Add(time.Second), End: base.Add(time.Second)}, Drop{Key: DropKey{ServiceID: "svc", AllocationID: "b", Reason: ReasonRateLimited}, Count: 2, Start: base, End: base}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("coalescing: %+v", rows)
	}
	for _, row := range rows {
		if row.GetAllocationId() == "a" && (row.GetDroppedCount() != 7 || !row.GetWindowStart().AsTime().Equal(base) || !row.GetWindowEnd().AsTime().Equal(base.Add(time.Second))) {
			t.Fatalf("lost window: %+v", row)
		}
	}
}

func TestDurableDropRetryAndConcurrentGrowth(t *testing.T) {
	dir := t.TempDir()
	s := openTestSpool(t, SpoolConfig{Dir: dir})
	base := time.Now()
	key := DropKey{AllocationID: "a", Reason: ReasonRateLimited}
	if err := s.AddDrops(Drop{Key: key, Count: 3, Start: base, End: base}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	id := snapshot[0].GetSummaryId()
	if err := s.AddDrops(Drop{Key: key, Count: 4, Start: base.Add(time.Second), End: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openTestSpool(t, SpoolConfig{Dir: dir})
	grown, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if len(grown) != 1 || grown[0].GetSummaryId() != id || grown[0].GetDroppedCount() != 7 {
		t.Fatalf("retry changed identity: %+v", grown)
	}
	// Only the original snapshot reached the sink. New losses cannot overwrite
	// its accepted count with a reduced value under the same ID.
	_, c := readSpool(t, s, 1)
	if err := s.Commit(c, snapshot); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].GetDroppedCount() != 4 || fresh[0].GetSummaryId() == id {
		t.Fatalf("snapshot acknowledgement lost concurrent additions: %+v", fresh)
	}
}

func TestDropAcknowledgementFailureDoesNotAdvanceCursor(t *testing.T) {
	s := openTestSpool(t, SpoolConfig{})
	mustAppend(t, s, testRecord("a", 1, time.Now()))
	if err := s.AddDrops(Drop{Key: DropKey{AllocationID: "a"}, Count: 1, Start: time.Now(), End: time.Now()}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	snapshot[0].DroppedCount = 2
	_, c := readSpool(t, s, 1)
	if err := s.Commit(c, snapshot); err == nil {
		t.Fatal("acknowledged nonexistent drops")
	}
	rows, c := readSpool(t, s, 1)
	if len(rows) != 1 || pendingDrops(t, s) != 1 {
		t.Fatal("failed ack skipped records or losses")
	}
	snapshot, err = s.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(c, snapshot); err != nil {
		t.Fatal(err)
	}
	if pendingDrops(t, s) != 0 || spoolStats(t, s).PendingRecords != 0 {
		t.Fatal("confirmed acknowledgement did not consume both")
	}
}

func TestBuildAbandonmentAndIdempotentTakeover(t *testing.T) {
	old := openTestSpool(t, SpoolConfig{})
	base := time.Now()
	for _, stream := range []string{"stdout", "stderr", "stdout"} {
		mustAppend(t, old, Record{DropKey: DropKey{ServiceID: "svc", BuildID: "build", LogType: platformv1.ServiceLogType_SERVICE_LOG_TYPE_BUILD, Stream: stream}, ObservedAt: base, Payload: []byte("lost")})
	}
	if err := old.Abandon(); err != nil {
		t.Fatal(err)
	}
	rows, err := old.PendingDrops()
	if err != nil {
		t.Fatal(err)
	}
	if pendingDrops(t, old) != 3 || spoolStats(t, old).Records != 0 || len(rows) != 2 {
		t.Fatalf("abandonment: %+v", rows)
	}
	fresh := openTestSpool(t, SpoolConfig{})
	for i := 0; i < 2; i++ {
		if err := fresh.MergeDrops(rows); err != nil {
			t.Fatal(err)
		}
	}
	if pendingDrops(t, fresh) != 3 {
		t.Fatal("takeover replay added counts twice")
	}
	if err := old.AcknowledgeDrops(rows); err != nil {
		t.Fatal(err)
	}
	if pendingDrops(t, old) != 0 || pendingDrops(t, fresh) != 3 {
		t.Fatal("takeover cleared the surviving copy")
	}
}
