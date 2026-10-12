package productionops

import (
	"database/sql"
	"testing"
	"time"
)

func TestBuilderDrainBlockingHonorsThePlanDeadline(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	drained := func(age time.Duration) sql.NullTime {
		return sql.NullTime{Time: now.Add(-age), Valid: true}
	}
	for _, tc := range []struct {
		name     string
		active   int
		drained  sql.NullTime
		deadline time.Duration
		blocked  bool
	}{
		{"no active builds", 0, sql.NullTime{}, 0, false},
		{"active builds wait indefinitely by default", 1, drained(time.Hour), 0, true},
		{"active builds inside the deadline", 1, drained(time.Minute), 10 * time.Minute, true},
		{"active builds past the deadline proceed", 1, drained(11 * time.Minute), 10 * time.Minute, false},
		{"unknown drain start never forces a restart", 1, sql.NullTime{}, 10 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := builderDrainBlocking(tc.active, tc.drained, now, tc.deadline); got != tc.blocked {
				t.Fatalf("builderDrainBlocking = %t, want %t", got, tc.blocked)
			}
		})
	}
}
