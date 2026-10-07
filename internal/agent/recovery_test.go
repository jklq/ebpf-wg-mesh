package agent

import (
	"testing"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
)

func TestRecoveryGenerationPreservesNewerInventoryAndFencesOldCommands(t *testing.T) {
	store := openTestLocalState(t)
	old := reconciliation.Authority{InstallationID: "installation", Generation: "before", ClusterID: "cluster-a"}
	if _, err := store.admitAuthority(old); err != nil {
		t.Fatal(err)
	}
	checkpoint := testDesiredState(900, 1000, "matching", "newer")
	checkpoint.InstallationId, checkpoint.RecoveryGeneration = old.InstallationID, old.Generation
	if _, err := store.acceptDesired("cluster-a", "test-session", checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.recordRuntimeInventory([]RuntimeResource{{AllocationID: "matching", RuntimeID: "matching-runtime"}, {AllocationID: "newer", RuntimeID: "newer-runtime"}}); err != nil {
		t.Fatal(err)
	}
	current := reconciliation.Authority{InstallationID: old.InstallationID, Generation: "after", ClusterID: "cluster-b", Paused: true}
	if changed, err := store.admitAuthority(current); err != nil || !changed {
		t.Fatalf("admit recovery: %v %v", changed, err)
	}
	summary, err := store.summary()
	if err != nil || len(summary.RuntimeResources) != 2 || len(summary.Allocations) != 2 || !summary.CheckpointRequired || summary.AuthorityEpoch != 0 {
		t.Fatalf("lost recovery inventory: %+v %v", summary, err)
	}
	checkpoint.ClusterId = current.ClusterID
	if _, err := store.acceptDesired(current.ClusterID, "test-session", checkpoint); err == nil {
		t.Fatal("prior generation authorized a command with a newer epoch")
	}
	for _, command := range []generationCommand{&agentv1.AllocationDiff{AuthorityEpoch: 900}, &agentv1.PullCredentialSet{AuthorityEpoch: 900}, &agentv1.NodeConfigUpdate{AuthorityEpoch: 900}, &agentv1.ReplicaEndpoints{AuthorityEpoch: 900}} {
		if err := store.observeAuthorityEpoch(command); err == nil {
			t.Fatal("prior-generation auxiliary command accepted")
		}
	}
	if _, err := store.acceptAllocationDiff(current.ClusterID, "test-session", &agentv1.AllocationDiff{}); err == nil {
		t.Fatal("incremental update preceded recovery checkpoint")
	}
	restored := testDesiredState(1, 2, "matching")
	restored.ClusterId, restored.InstallationId, restored.RecoveryGeneration = current.ClusterID, current.InstallationID, current.Generation
	if _, err := store.acceptDesired(current.ClusterID, "test-session", restored); err != nil {
		t.Fatal("restored checkpoint could not replace newer position", err)
	}
	summary, _ = store.summary()
	if summary.CheckpointRequired || summary.AuthorityEpoch != 1 || summary.ReconciliationCursor != 2 || len(summary.RuntimeResources) != 2 || summary.Initialization != initializationRecovery {
		t.Fatalf("unknown allocation was not quarantined: %+v", summary)
	}
	if changed, err := store.admitAuthority(current); err != nil || changed {
		t.Fatal("repetition reset the recovery generation", changed, err)
	}
	if _, err := store.admitAuthority(old); err == nil {
		t.Fatal("retired generation was readmitted")
	}
	if err := store.validateRecords(); err != nil {
		t.Fatal("interrupted recovery cannot reopen its local store", err)
	}
}

func TestRecoveryCheckpointOnFreshAgent(t *testing.T) {
	store := openTestLocalState(t)
	a := reconciliation.Authority{InstallationID: "installation", Generation: "fresh", ClusterID: "cluster-a"}
	if _, err := store.admitAuthority(a); err != nil {
		t.Fatal(err)
	}
	state := testDesiredState(1, 1, "matching")
	state.InstallationId, state.RecoveryGeneration = a.InstallationID, a.Generation
	if _, err := store.acceptDesired(a.ClusterID, "test-session", state); err != nil {
		t.Fatal(err)
	}
	summary, _ := store.summary()
	if summary.Initialization != initializationReady {
		t.Fatal("fresh agent did not become ready after checkpoint")
	}
}
