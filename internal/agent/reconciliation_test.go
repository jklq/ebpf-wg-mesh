package agent

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestGrantExpiryDuringPersistenceCannotChangeAcceptedState(t *testing.T) {
	for _, removal := range []bool{true, false} {
		name := "resurrection"
		if removal {
			name = "removal"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := openLocalStateStore(dir, "node-1")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			accepted, candidate := testDesiredState(1, 1), testDesiredState(1, 2)
			if removal {
				accepted = testDesiredState(1, 1, "allocation")
			} else {
				candidate = testDesiredState(1, 2, "allocation")
			}
			if _, err := store.acceptDesired("cluster-a", "test-session", accepted); err != nil {
				t.Fatal(err)
			}
			var allocationBefore []byte
			if err := store.db.View(func(tx *bbolt.Tx) error {
				allocationBefore = bytes.Clone(tx.Bucket(localAllocationsBucket).Get([]byte("allocation")))
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			issued := time.Now()
			candidate.AuthorityNotAfter = timestamppb.New(issued.Add(reconciliation.GrantLifetime))
			checks := 0
			store.now = func() time.Time {
				checks++
				if checks == 1 {
					return issued
				}
				if err := store.db.View(func(tx *bbolt.Tx) error {
					var staged agentv1.DesiredNodeState
					if err := proto.Unmarshal(tx.Bucket(localDesiredBucket).Get(stagedDesiredStateKey), &staged); err != nil {
						return err
					}
					if !proto.Equal(&staged, candidate) || readInt64(tx.Bucket(localMetaBucket).Get(cursorKey)) != 1 {
						t.Error("grant decision did not follow durable staging of the exact candidate")
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
				return candidate.AuthorityNotAfter.AsTime().Add(-reconciliation.MaxClockSkew)
			}
			if changed, err := store.acceptDesired("cluster-a", "test-session", candidate); err == nil || changed {
				t.Fatalf("expired persistence was accepted: changed=%v err=%v", changed, err)
			}
			if checks != 2 {
				t.Fatalf("grant checks = %d, want before and after durable staging", checks)
			}
			assertUnchanged := func(store *localStateStore) {
				t.Helper()
				desired, err := store.desiredState()
				if err != nil || !proto.Equal(desired, accepted) {
					t.Fatalf("expired candidate changed accepted state or credentials: %v %v", desired, err)
				}
				summary, err := store.summary()
				if err != nil || summary.AuthorityEpoch != 1 || summary.ReconciliationCursor != 1 {
					t.Fatalf("expired candidate changed hello position: %+v %v", summary, err)
				}
				if err := store.db.View(func(tx *bbolt.Tx) error {
					if !bytes.Equal(tx.Bucket(localAllocationsBucket).Get([]byte("allocation")), allocationBefore) {
						t.Error("expired candidate changed allocation operations")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertUnchanged(store)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := openLocalStateStore(dir, "node-1")
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			assertUnchanged(restored)
			candidate.AuthorityNotAfter = timestamppb.New(time.Now().Add(reconciliation.GrantLifetime))
			if changed, err := restored.acceptDesired("cluster-a", "test-session", candidate); err != nil || !changed {
				t.Fatalf("fresh grant did not recover: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestRestartDoesNotAcceptStagedSnapshot(t *testing.T) {
	store := openTestLocalState(t)
	accepted := testDesiredState(1, 1, "allocation")
	if _, err := store.acceptDesired("cluster-a", "test-session", accepted); err != nil {
		t.Fatal(err)
	}
	staged, err := store.stageDesired("test-session", testDesiredState(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(store.path)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if desired, err := restored.desiredState(); err != nil || !proto.Equal(desired, accepted) {
		t.Fatalf("restart promoted a staged removal: %v %v", desired, err)
	}
	if _, err := restored.acceptStagedDesired("cluster-a", "test-session", staged); err == nil {
		t.Fatal("restart retained an unaccepted candidate")
	}
}

func TestRejectedScopeCannotRemoveAllocationsAndObservedEpochSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(4, 3, "writer")); err != nil {
		t.Fatal(err)
	}
	incomplete := testDesiredState(5, 0)
	incomplete.Complete = false
	if _, err := store.acceptDesired("cluster-a", "test-session", incomplete); err == nil {
		t.Fatal("accepted incomplete scope")
	}
	desired, err := store.desiredState()
	if err != nil || len(desired.GetServices()) != 1 {
		t.Fatalf("rejection changed accepted state: %v, %v", desired, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(4, 99)); err == nil {
		t.Fatal("restart forgot higher observed epoch")
	}
	unsupported := testDesiredState(5, 1)
	unsupported.Scope = agentv1.SnapshotScope_SNAPSHOT_SCOPE_UNSPECIFIED
	if _, err := store.acceptDesired("cluster-a", "test-session", unsupported); err == nil {
		t.Fatal("accepted unspecified scope")
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(5, 1)); err != nil {
		t.Fatal(err)
	}
	desired, err = store.desiredState()
	if err != nil || len(desired.GetServices()) != 0 {
		t.Fatalf("complete scope did not remove desired allocation: %v", err)
	}
}

func TestClusterMismatchRequiresExplicitRecoveryAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-b", "test-session", testDesiredState(2, 0)); err == nil {
		t.Fatal("accepted new cluster")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	summary, err := store.summary()
	if err != nil || summary.ClusterIdentity != "cluster-a" || summary.Initialization != initializationRecovery {
		t.Fatalf("identity recovery not persisted: %+v %v", summary, err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(2, 0)); err == nil {
		t.Fatal("routine reconnect cleared identity recovery")
	}
}

func TestRemovedAllocationCannotReturnThroughDuplicateOrDelayedSnapshot(t *testing.T) {
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	old := testDesiredState(1, 1, "allocation")
	removed := testDesiredState(1, 2)
	for _, desired := range []*agentv1.DesiredNodeState{old, removed, removed} {
		if _, err := store.acceptDesired("cluster-a", "test-session", desired); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", old); err == nil {
		t.Fatal("delayed snapshot resurrected allocation")
	}
	reconnect := testDesiredState(1, 2)
	reconnect.SessionId = "reconnected"
	if changed, err := store.acceptDesired("cluster-a", "reconnected", reconnect); err != nil || changed {
		t.Fatalf("duplicate on reconnect: changed=%v err=%v", changed, err)
	}
	if _, err := store.acceptDesired("cluster-a", "reconnected", removed); err == nil {
		t.Fatal("old session accepted on reconnect")
	}
	expired := testDesiredState(1, 3, "allocation")
	expired.SessionId = "reconnected"
	expired.AuthorityNotAfter = timestamppb.New(time.Now().Add(-time.Second))
	if _, err := store.acceptDesired("cluster-a", "reconnected", expired); err == nil {
		t.Fatal("expired authority resurrected allocation")
	}
	path := store.path
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := openLocalStateStore(filepath.Dir(path), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.acceptDesired("cluster-a", "test-session", old); err == nil {
		t.Fatal("restart resurrected removed allocation")
	}
	state, err := restored.desiredState()
	if err != nil || state.GetReconciliationCursor() != 2 || len(state.GetServices()) != 0 {
		t.Fatalf("removal was not retained: %v %v", state, err)
	}
}

func TestInterruptedDesiredTransactionRetainsSnapshotAndCursor(t *testing.T) {
	store := openTestLocalState(t)
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "allocation")); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(localAllocationsBucket).Put([]byte("allocation"), []byte("broken"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 2, "allocation")); err == nil {
		t.Fatal("failed persistence acknowledged acceptance")
	}
	state, err := store.desiredState()
	summary, summaryErr := store.db.Begin(false)
	if summaryErr != nil {
		t.Fatal(summaryErr)
	}
	defer summary.Rollback()
	if err != nil || state.GetReconciliationCursor() != 1 || len(state.GetServices()) != 1 || readInt64(summary.Bucket(localMetaBucket).Get(cursorKey)) != 1 {
		t.Fatalf("failed commit changed desired state: %v %v", state, err)
	}
}

func TestAcceptancePersistsBeforeRuntimeProgressAndSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareStartup("cluster-a", nil); err != nil {
		t.Fatal(err)
	}
	first, err := store.nextSessionIncarnation()
	if err != nil {
		t.Fatal(err)
	}
	runtime := &supervisorTestRuntime{}
	supervisor := newWorkloadSupervisor("node-1", runtime, store, nil)
	if changed, err := supervisor.AcceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "allocation")); err != nil || !changed {
		t.Fatalf("acceptance: %v %v", changed, err)
	}
	if report, err := supervisor.CurrentReport(); err != nil || report != nil || runtime.calls != 0 {
		t.Fatalf("acceptance ran workload: %v %v calls=%d", report, err, runtime.calls)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openLocalStateStore(dir, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := store.nextSessionIncarnation()
	if err != nil || second != first+1 {
		t.Fatalf("session incarnation reused after restart: %d %d %v", first, second, err)
	}
	desired, err := store.desiredState()
	if err != nil || desired.GetReconciliationCursor() != 1 || len(desired.GetServices()) != 1 {
		t.Fatalf("acceptance was not durable: %v %v", desired, err)
	}
}
