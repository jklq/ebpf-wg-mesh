//go:build integration

package durablework

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach-go/v2/crdb"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"

	"ebof-wg-mesh/internal/testdb"
)

// testSchema mirrors the durable_work_items table in
// internal/controlplane/store_schema.go. The control-plane suite exercises
// the migrated DDL; this file keeps the package suite self-contained.
const testSchema = `CREATE TABLE durable_work_items (
	id STRING PRIMARY KEY,
	kind STRING NOT NULL,
	dedup_key STRING NOT NULL UNIQUE,
	resource_type STRING NOT NULL DEFAULT '',
	resource_id STRING NOT NULL DEFAULT '',
	state STRING NOT NULL CHECK (state IN ('pending', 'leased', 'succeeded', 'failed', 'dead')),
	attempt_count INT8 NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	attempt_limit INT8 NOT NULL CHECK (attempt_limit > 0),
	owner_id STRING NOT NULL DEFAULT '',
	owner_epoch INT8 NOT NULL DEFAULT 0 CHECK (owner_epoch >= 0),
	lease_expires_at TIMESTAMPTZ NULL,
	last_error STRING NOT NULL DEFAULT '',
	available_at TIMESTAMPTZ NOT NULL,
	payload JSONB NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NULL
)`

const testEffectSchema = `
CREATE TABLE test_effect_intents (
	dedup_key STRING PRIMARY KEY,
	completed BOOL NOT NULL DEFAULT FALSE
);
CREATE TABLE test_effect_runs (
	id STRING PRIMARY KEY,
	dedup_key STRING NOT NULL,
	ran_at TIMESTAMPTZ NOT NULL
)`

var (
	testServerOnce sync.Once
	testServer     testserver.TestServer
	testServerErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testServer != nil {
		testServer.Stop()
	}
	os.Exit(code)
}

func openTestStore(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	testServerOnce.Do(func() {
		testServer, testServerErr = testdb.Start("")
	})
	if testServerErr != nil {
		t.Fatalf("start test server: %v", testServerErr)
	}
	adminDB, err := sql.Open("pgx", testServer.PGURL().String())
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()
	dbName := "dw_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	pgURL := testdb.NormalizeURL(testServer.PGURL())
	pgURL.Path = "/" + dbName
	db, err := sql.Open("pgx", pgURL.String())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), testSchema); err != nil {
		t.Fatalf("create work table: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), testEffectSchema); err != nil {
		t.Fatalf("create effect tables: %v", err)
	}
	withTx := func(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
		return crdb.ExecuteTx(ctx, db, nil, func(tx *sql.Tx) error { return fn(ctx, tx) })
	}
	return db, NewStore(db, withTx)
}

func testParams(dedup string) EnqueueParams {
	return EnqueueParams{
		Kind:         "test-kind",
		DedupKey:     dedup,
		ResourceType: "test-resource",
		ResourceID:   dedup,
		Payload:      []byte(`{"dedup":"` + dedup + `"}`),
	}
}

func expireLease(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE durable_work_items SET lease_expires_at = statement_timestamp() - INTERVAL '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func backdateAvailable(t *testing.T, db *sql.DB, id string, ago time.Duration) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE durable_work_items SET available_at = statement_timestamp() - $1::INT8 * INTERVAL '1 microsecond' WHERE id = $2`,
		ago.Microseconds(), id); err != nil {
		t.Fatalf("backdate available_at: %v", err)
	}
}

func TestEnqueueDedupsWhileActiveAndResurrectsWhenTerminal(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	enqueued, err := queue.Enqueue(ctx, testParams("dedup-1"))
	if err != nil || !enqueued {
		t.Fatalf("first enqueue = (%v, %v), want (true, nil)", enqueued, err)
	}
	enqueued, err = queue.Enqueue(ctx, testParams("dedup-1"))
	if err != nil || enqueued {
		t.Fatalf("duplicate enqueue = (%v, %v), want (false, nil)", enqueued, err)
	}
	rec, err := queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || rec.ID == "" {
		t.Fatalf("claim = (%+v, %v)", rec, err)
	}
	if rec.AttemptCount != 1 || rec.OwnerEpoch != 1 {
		t.Fatalf("first claim attempt=%d epoch=%d, want 1/1", rec.AttemptCount, rec.OwnerEpoch)
	}
	if err := queue.Fail(ctx, rec, errors.New("boom"), FailOptions{Retryable: true}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	enqueued, err = queue.Enqueue(ctx, testParams("dedup-1"))
	if err != nil || enqueued {
		t.Fatalf("enqueue while pending = (%v, %v), want (false, nil)", enqueued, err)
	}
	backdateAvailable(t, db, rec.ID, time.Second)
	rec, err = queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || rec.ID == "" {
		t.Fatalf("reclaim = (%+v, %v)", rec, err)
	}
	if rec.AttemptCount != 2 || rec.OwnerEpoch != 2 {
		t.Fatalf("second claim attempt=%d epoch=%d, want 2/2", rec.AttemptCount, rec.OwnerEpoch)
	}
	if err := queue.Complete(ctx, rec); err != nil {
		t.Fatalf("complete: %v", err)
	}
	enqueued, err = queue.Enqueue(ctx, testParams("dedup-1"))
	if err != nil || !enqueued {
		t.Fatalf("enqueue after terminal = (%v, %v), want (true, nil)", enqueued, err)
	}
	got, err := queue.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get resurrected: %v", err)
	}
	if got.State != StatePending || got.AttemptCount != 0 || got.OwnerID != "" {
		t.Fatalf("resurrected = %+v, want fresh pending", got)
	}
}

func TestEnqueueTxCommitsWithTheCallerTransaction(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	rolledBack := func() bool {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		enqueued, err := queue.EnqueueTx(ctx, tx, testParams("tx-1"))
		if err != nil || !enqueued {
			t.Fatalf("enqueue tx = (%v, %v)", enqueued, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO test_effect_intents(dedup_key, completed) VALUES ('tx-1', FALSE)`); err != nil {
			t.Fatalf("marker insert: %v", err)
		}
		// Deliberate rollback: neither the work row nor the marker survives.
		return true
	}()
	if !rolledBack {
		t.Fatal("unreachable")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM durable_work_items WHERE dedup_key = 'tx-1'`).Scan(&count); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d work rows survived rollback", count)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM test_effect_intents WHERE dedup_key = 'tx-1'`).Scan(&count); err != nil {
		t.Fatalf("count markers after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d marker rows survived rollback", count)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	enqueued, err := queue.EnqueueTx(ctx, tx, testParams("tx-1"))
	if err != nil || !enqueued {
		_ = tx.Rollback()
		t.Fatalf("enqueue tx = (%v, %v)", enqueued, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO test_effect_intents(dedup_key, completed) VALUES ('tx-1', FALSE)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("marker insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM durable_work_items WHERE dedup_key = 'tx-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("work rows after commit = %d, %v", count, err)
	}
}

func TestConcurrentClaimsElectASingleWinner(t *testing.T) {
	t.Parallel()
	_, queue := openTestStore(t)
	ctx := context.Background()

	if _, err := queue.Enqueue(ctx, testParams("race-1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	const racers = 8
	start := make(chan struct{})
	type outcome struct {
		rec Record
		err error
	}
	results := make(chan outcome, racers)
	for i := 0; i < racers; i++ {
		go func(n int) {
			<-start
			rec, err := queue.Claim(ctx, fmt.Sprintf("racer-%d", n), time.Minute)
			results <- outcome{rec: rec, err: err}
		}(i)
	}
	close(start)
	winners := 0
	for range racers {
		result := <-results
		if result.err != nil {
			t.Fatalf("claim: %v", result.err)
		}
		if result.rec.ID != "" {
			winners++
			if result.rec.AttemptCount != 1 || result.rec.OwnerEpoch != 1 {
				t.Fatalf("winner attempt=%d epoch=%d, want 1/1", result.rec.AttemptCount, result.rec.OwnerEpoch)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want exactly 1", winners)
	}
}

func TestConcurrentWorkersClaimDisjointRecords(t *testing.T) {
	t.Parallel()
	_, queue := openTestStore(t)
	ctx := context.Background()

	const records = 4
	for i := 0; i < records; i++ {
		if _, err := queue.Enqueue(ctx, testParams(fmt.Sprintf("disjoint-%d", i))); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	const workers = 4
	var mu sync.Mutex
	claimed := map[string]string{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", n)
			for range records {
				rec, err := queue.Claim(ctx, owner, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if rec.ID == "" {
					return
				}
				mu.Lock()
				if prev, dup := claimed[rec.ID]; dup {
					t.Errorf("record %s claimed by %s and %s", rec.ID, prev, owner)
				}
				claimed[rec.ID] = owner
				mu.Unlock()
				if err := queue.Complete(ctx, rec); err != nil {
					t.Errorf("complete: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if len(claimed) != records {
		t.Fatalf("%d distinct records claimed, want %d", len(claimed), records)
	}
}

func TestStalledOwnerCannotCommitAfterTakeover(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	if _, err := queue.Enqueue(ctx, testParams("takeover-1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	stalled, err := queue.Claim(ctx, "owner-stalled", time.Minute)
	if err != nil || stalled.ID == "" {
		t.Fatalf("claim = (%+v, %v)", stalled, err)
	}
	// A live lease is not takeable.
	if other, err := queue.Claim(ctx, "owner-next", time.Minute); err != nil || other.ID != "" {
		t.Fatalf("live lease claim = (%+v, %v), want empty", other, err)
	}
	expireLease(t, db, stalled.ID)
	current, err := queue.Claim(ctx, "owner-next", time.Minute)
	if err != nil || current.ID != stalled.ID {
		t.Fatalf("takeover = (%+v, %v), want %s", current, err, stalled.ID)
	}
	if current.OwnerEpoch != stalled.OwnerEpoch+1 {
		t.Fatalf("takeover epoch = %d, want %d", current.OwnerEpoch, stalled.OwnerEpoch+1)
	}
	if err := queue.Complete(ctx, stalled); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale complete = %v, want ErrLeaseLost", err)
	}
	if err := queue.Heartbeat(ctx, stalled, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat = %v, want ErrLeaseLost", err)
	}
	if err := queue.Fail(ctx, stalled, errors.New("late"), FailOptions{Retryable: true}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale fail = %v, want ErrLeaseLost", err)
	}
	// The current owner is unaffected by the stale attempts.
	if err := queue.Heartbeat(ctx, current, time.Minute); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if err := queue.Complete(ctx, current); err != nil {
		t.Fatalf("current complete: %v", err)
	}
	got, err := queue.Get(ctx, current.ID)
	if err != nil || got.State != StateSucceeded {
		t.Fatalf("terminal state = (%+v, %v), want succeeded", got, err)
	}
}

// runEffectfulHandler simulates the documented external-effect pattern: the
// worker persists an intent row before the effect and marks it completed
// after, so a takeover never repeats an effect the dead worker performed.
func runEffectfulHandler(ctx context.Context, t *testing.T, db *sql.DB, queue *Store, owner, dedup string) (claimed bool) {
	t.Helper()
	rec, err := queue.Claim(ctx, owner, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if rec.ID == "" {
		return false
	}
	var completed bool
	if err := db.QueryRowContext(ctx, `SELECT completed FROM test_effect_intents WHERE dedup_key = $1`, dedup).Scan(&completed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read intent: %v", err)
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.ExecContext(ctx, `INSERT INTO test_effect_intents(dedup_key, completed) VALUES ($1, FALSE)`, dedup); err != nil {
			t.Fatalf("persist intent: %v", err)
		}
	}
	if !completed {
		if _, err := db.ExecContext(ctx, `INSERT INTO test_effect_runs(id, dedup_key, ran_at) VALUES ($1, $2, statement_timestamp())`, uuid.NewString(), dedup); err != nil {
			t.Fatalf("perform effect: %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE test_effect_intents SET completed = TRUE WHERE dedup_key = $1`, dedup); err != nil {
			t.Fatalf("record outcome: %v", err)
		}
	}
	if err := queue.Complete(ctx, rec); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return true
}

func countEffectRuns(t *testing.T, db *sql.DB, dedup string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM test_effect_runs WHERE dedup_key = $1`, dedup).Scan(&count); err != nil {
		t.Fatalf("count effect runs: %v", err)
	}
	return count
}

func TestWorkerDeathBeforeAndAfterTheExternalEffect(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	// Death before the effect: the first worker claims and vanishes without
	// touching the effect. The takeover runs the handler exactly once.
	if _, err := queue.Enqueue(ctx, testParams("death-before")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dead, err := queue.Claim(ctx, "owner-doomed", time.Minute)
	if err != nil || dead.ID == "" {
		t.Fatalf("claim = (%+v, %v)", dead, err)
	}
	expireLease(t, db, dead.ID)
	if !runEffectfulHandler(ctx, t, db, queue, "owner-heir", "death-before") {
		t.Fatal("takeover claimed nothing")
	}
	if got := countEffectRuns(t, db, "death-before"); got != 1 {
		t.Fatalf("effect ran %d times, want once", got)
	}

	// Death after the effect but before completion: the first worker
	// performs the effect and records the outcome, then vanishes. The
	// takeover observes the completed intent and does not repeat it.
	if _, err := queue.Enqueue(ctx, testParams("death-after")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rec, err := queue.Claim(ctx, "owner-doomed", time.Minute)
	if err != nil || rec.ID == "" {
		t.Fatalf("claim = (%+v, %v)", rec, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO test_effect_intents(dedup_key, completed) VALUES ('death-after', FALSE)`); err != nil {
		t.Fatalf("persist intent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO test_effect_runs(id, dedup_key, ran_at) VALUES ($1, 'death-after', statement_timestamp())`, uuid.NewString()); err != nil {
		t.Fatalf("perform effect: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE test_effect_intents SET completed = TRUE WHERE dedup_key = 'death-after'`); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	expireLease(t, db, rec.ID)
	if !runEffectfulHandler(ctx, t, db, queue, "owner-heir", "death-after") {
		t.Fatal("takeover claimed nothing")
	}
	if got := countEffectRuns(t, db, "death-after"); got != 1 {
		t.Fatalf("effect ran %d times, want once", got)
	}
}

func TestRetryableFailuresDeadLetterAndStayInspectable(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	params := testParams("retry-1")
	params.AttemptLimit = 2
	if _, err := queue.Enqueue(ctx, params); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first, err := queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || first.ID == "" {
		t.Fatalf("first claim = (%+v, %v)", first, err)
	}
	if err := queue.Fail(ctx, first, errors.New("transient"), FailOptions{Retryable: true}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, err := queue.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StatePending || got.LastError != "transient" {
		t.Fatalf("after retryable fail = %+v", got)
	}
	if !got.AvailableAt.After(got.UpdatedAt) {
		t.Fatalf("expected backoff available_at after updated_at, got %+v", got)
	}
	// Nothing is due until the backoff elapses.
	if rec, err := queue.Claim(ctx, "owner-b", time.Minute); err != nil || rec.ID != "" {
		t.Fatalf("backoff claim = (%+v, %v), want empty", rec, err)
	}
	backdateAvailable(t, db, first.ID, time.Second)
	second, err := queue.Claim(ctx, "owner-b", time.Minute)
	if err != nil || second.ID != first.ID {
		t.Fatalf("second claim = (%+v, %v)", second, err)
	}
	if second.AttemptCount != 2 {
		t.Fatalf("second claim attempt = %d, want 2", second.AttemptCount)
	}
	if err := queue.Fail(ctx, second, errors.New("still broken"), FailOptions{Retryable: true}); err != nil {
		t.Fatalf("final fail: %v", err)
	}
	got, err = queue.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateDead || got.LastError != "still broken" || !got.CompletedAt.Valid {
		t.Fatalf("after exhausted fail = %+v, want dead", got)
	}
	dead, err := queue.ListDead(ctx, "", 10)
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != first.ID {
		t.Fatalf("dead list = %+v, want the exhausted record", dead)
	}
	filtered, err := queue.ListDead(ctx, "other-kind", 10)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("filtered dead list = (%+v, %v), want empty", filtered, err)
	}
	if lag, err := queue.QueueLag(ctx); err != nil || lag != 0 {
		t.Fatalf("lag with only dead rows = (%v, %v), want 0", lag, err)
	}
}

func TestNonRetryableFailuresMoveStraightToFailed(t *testing.T) {
	t.Parallel()
	_, queue := openTestStore(t)
	ctx := context.Background()

	if _, err := queue.Enqueue(ctx, testParams("poison-1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rec, err := queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || rec.ID == "" {
		t.Fatalf("claim = (%+v, %v)", rec, err)
	}
	if err := queue.Fail(ctx, rec, errors.New("poison payload"), FailOptions{}); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, err := queue.Get(ctx, rec.ID)
	if err != nil || got.State != StateFailed {
		t.Fatalf("after poison fail = (%+v, %v), want failed", got, err)
	}
	dead, err := queue.ListDead(ctx, "", 10)
	if err != nil {
		t.Fatalf("list dead: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("failed record listed as dead: %+v", dead)
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	if _, err := queue.Enqueue(ctx, testParams("heartbeat-1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rec, err := queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || rec.ID == "" {
		t.Fatalf("claim = (%+v, %v)", rec, err)
	}
	before := rec.LeaseExpiresAt.Time
	if err := queue.Heartbeat(ctx, rec, 10*time.Minute); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, err := queue.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LeaseExpiresAt.Time.After(before.Add(5 * time.Minute)) {
		t.Fatalf("heartbeat did not extend the lease: %v -> %v", before, got.LeaseExpiresAt.Time)
	}
	// A heartbeat cannot steal a lease it does not hold.
	expireLease(t, db, rec.ID)
	taken, err := queue.Claim(ctx, "owner-b", time.Minute)
	if err != nil || taken.ID != rec.ID {
		t.Fatalf("takeover = (%+v, %v)", taken, err)
	}
	if err := queue.Heartbeat(ctx, rec, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat = %v, want ErrLeaseLost", err)
	}
}

func TestQueueLagTracksTheOldestDueRecord(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	if lag, err := queue.QueueLag(ctx); err != nil || lag != 0 {
		t.Fatalf("empty lag = (%v, %v), want 0", lag, err)
	}
	future := testParams("lag-future")
	future.AvailableAt = time.Now().UTC().Add(time.Hour)
	if _, err := queue.Enqueue(ctx, future); err != nil {
		t.Fatalf("enqueue future: %v", err)
	}
	if lag, err := queue.QueueLag(ctx); err != nil || lag != 0 {
		t.Fatalf("not-due lag = (%v, %v), want 0", lag, err)
	}
	if _, err := queue.Enqueue(ctx, testParams("lag-due")); err != nil {
		t.Fatalf("enqueue due: %v", err)
	}
	var dueID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM durable_work_items WHERE dedup_key = 'lag-due'`).Scan(&dueID); err != nil {
		t.Fatalf("find due record: %v", err)
	}
	backdateAvailable(t, db, dueID, 90*time.Second)
	lag, err := queue.QueueLag(ctx)
	if err != nil {
		t.Fatalf("lag: %v", err)
	}
	if lag < 60*time.Second || lag > 120*time.Second {
		t.Fatalf("lag = %v, want ~90s", lag)
	}
	if lag, err := queue.QueueLag(ctx, "other-kind"); err != nil || lag != 0 {
		t.Fatalf("filtered lag = (%v, %v), want 0", lag, err)
	}
	rec, err := queue.Claim(ctx, "owner-a", time.Minute)
	if err != nil || rec.DedupKey != "lag-due" {
		t.Fatalf("claim = (%+v, %v), want the due record", rec, err)
	}
	if lag, err := queue.QueueLag(ctx); err != nil || lag != 0 {
		t.Fatalf("lag after claim = (%v, %v), want 0", lag, err)
	}
}

func TestPruneTerminalDeletesOnlyOldTerminalRows(t *testing.T) {
	t.Parallel()
	db, queue := openTestStore(t)
	ctx := context.Background()

	// Complete two records, then enqueue a third that stays live and must
	// survive pruning.
	for _, dedup := range []string{"prune-old", "prune-recent"} {
		if _, err := queue.Enqueue(ctx, testParams(dedup)); err != nil {
			t.Fatalf("enqueue %s: %v", dedup, err)
		}
	}
	for range 2 {
		rec, err := queue.Claim(ctx, "owner-a", time.Minute)
		if err != nil || rec.ID == "" {
			t.Fatalf("claim = (%+v, %v)", rec, err)
		}
		if err := queue.Complete(ctx, rec); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	if _, err := queue.Enqueue(ctx, testParams("prune-live")); err != nil {
		t.Fatalf("enqueue prune-live: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE durable_work_items SET completed_at = statement_timestamp() - INTERVAL '48 hours' WHERE dedup_key = 'prune-old'`); err != nil {
		t.Fatalf("age terminal row: %v", err)
	}
	pruned, err := queue.PruneTerminal(ctx, time.Now().UTC().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d rows, want 1", pruned)
	}
	byDedup := func(dedup string) (Record, error) {
		var id string
		if err := db.QueryRowContext(ctx, `SELECT id FROM durable_work_items WHERE dedup_key = $1`, dedup).Scan(&id); err != nil {
			return Record{}, err
		}
		return queue.Get(ctx, id)
	}
	if _, err := byDedup("prune-old"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old terminal row still present: %v", err)
	}
	if rec, err := byDedup("prune-recent"); err != nil || rec.State != StateSucceeded {
		t.Fatalf("recent terminal row = (%+v, %v), want succeeded", rec, err)
	}
	if rec, err := byDedup("prune-live"); err != nil || rec.State != StatePending {
		t.Fatalf("live row = (%+v, %v), want pending", rec, err)
	}
}
