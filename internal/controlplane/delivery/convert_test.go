package delivery

import (
	"database/sql"
	"testing"
	"time"
)

func TestToProtoTimestampOmitsAbsentValues(t *testing.T) {
	t.Parallel()

	if got := ToProtoTimestamp(time.Time{}); got != nil {
		t.Fatalf("zero time = %v, want nil", got)
	}
	if got := ToProtoTimestamp(time.Unix(0, 0).UTC()); got != nil {
		t.Fatalf("epoch = %v, want nil", got)
	}
	epochLocal := time.Unix(0, 0).In(time.FixedZone("CET", 3600))
	if got := ToProtoTimestamp(epochLocal); got != nil {
		t.Fatalf("epoch in non-UTC zone = %v, want nil", got)
	}
}

func TestToProtoTimestampPreservesValidInstantsInUTC(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("PST", -8*3600)
	input := time.Date(2026, 4, 24, 11, 0, 0, 123456789, zone)
	got := ToProtoTimestamp(input)
	if got == nil {
		t.Fatal("valid time produced nil timestamp")
	}
	if !got.IsValid() {
		t.Fatalf("timestamp %v failed validity", got)
	}
	roundTrip := got.AsTime()
	if !roundTrip.Equal(input) {
		t.Fatalf("round trip = %v, want %v", roundTrip, input)
	}
	if roundTrip.Location() != time.UTC {
		t.Fatalf("round trip zone = %v, want UTC", roundTrip.Location())
	}
}

func TestToProtoTimestampKeepsFutureInstants(t *testing.T) {
	t.Parallel()

	future := time.Now().UTC().Add(time.Hour)
	got := ToProtoTimestamp(future)
	if got == nil {
		t.Fatal("future time produced nil timestamp")
	}
	if !got.AsTime().Equal(future) {
		t.Fatalf("future round trip = %v, want %v", got.AsTime(), future)
	}
}

func TestToProtoTimestampRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	farFuture := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := ToProtoTimestamp(farFuture); got != nil {
		t.Fatalf("year 10000 = %v, want nil", got)
	}
}

func TestToProtoBuildStatusOmitsAbsentTimestamps(t *testing.T) {
	t.Parallel()

	queued := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	status := ToProtoBuildStatus(BuildRunRecord{ID: "build-1", State: "queued", QueuedAt: queued})
	if status == nil {
		t.Fatal("build status is nil")
	}
	if status.StartedAt != nil || status.FinishedAt != nil {
		t.Fatalf("unset build times should stay absent: %+v", status)
	}
	if status.QueuedAt == nil || !status.QueuedAt.AsTime().Equal(queued) {
		t.Fatalf("queued at = %v, want %v", status.QueuedAt, queued)
	}
}

func TestToProtoBuildStatusTreatsEpochStartAsAbsent(t *testing.T) {
	t.Parallel()

	status := ToProtoBuildStatus(BuildRunRecord{
		ID:        "build-1",
		State:     "running",
		QueuedAt:  time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
		StartedAt: sql.NullTime{Time: time.Unix(0, 0).UTC(), Valid: true},
	})
	if status == nil {
		t.Fatal("build status is nil")
	}
	if status.StartedAt != nil {
		t.Fatalf("epoch start = %v, want nil", status.StartedAt)
	}
}

func TestToProtoBuildStatusOmitsMissingBuild(t *testing.T) {
	t.Parallel()

	if got := ToProtoBuildStatus(BuildRunRecord{}); got != nil {
		t.Fatalf("empty build = %v, want nil", got)
	}
}

func TestAgentHealthyRejectsUnknownLastSeen(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	unknown := AgentRecord{ID: "agent", LifecycleState: AgentStateActive}
	if unknown.Healthy(now) {
		t.Fatal("agent with unknown last-seen reports healthy")
	}
	recent := AgentRecord{ID: "agent", LifecycleState: AgentStateActive, LastSeenAt: now.Add(-time.Second)}
	if !recent.Healthy(now) {
		t.Fatal("recently seen agent reports unhealthy")
	}
	stale := AgentRecord{ID: "agent", LifecycleState: AgentStateActive, LastSeenAt: now.Add(-time.Hour)}
	if stale.Healthy(now) {
		t.Fatal("stale agent reports healthy")
	}
}
