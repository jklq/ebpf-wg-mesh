package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/signkeys/signkeystest"
	"ebof-wg-mesh/internal/health"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/recovery"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecoveryGenerationPreservesNewerInventoryAndFencesOldCommands(t *testing.T) {
	store := openTestLocalState(t)
	old := reconciliation.Authority{InstallationID: "installation", Generation: "before", ClusterID: "cluster-a"}
	if _, err := store.admitAuthority(old); err != nil {
		t.Fatal(err)
	}
	checkpoint := testDesiredState(900, 1000, "matching", "newer")
	checkpoint.NodeConfig.WireguardAddresses = []string{"fd00:44::11/64"}
	checkpoint.NodeConfig.WorkloadIpv6Subnet = "fd00:8::/64"
	created := time.Now().Add(-time.Hour).UTC()
	checkpoint.Services[0].CreatedAt = timestamppb.New(created)
	checkpoint.Services[0].PrivateIpv4 = "10.0.0.2"
	checkpoint.Services[0].NetworkIdentity = 700
	checkpoint.NodeConfigVersion = reconciliation.HashNodeConfig(checkpoint.NodeConfig)
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
	inventory, err := store.fleetInventory()
	if err != nil || len(inventory.Allocations) != 2 || inventory.Generation != current.Generation {
		t.Fatal("recovery lost external inventory", inventory, err)
	}
	allocation := inventory.Allocations[slices.IndexFunc(inventory.Allocations, func(a recovery.FleetAllocation) bool { return a.ID == "matching" })]
	if allocation.ServiceID != "service-matching" || allocation.EnvironmentID != "env-1" || allocation.IPv4 != "10.0.0.2" || !allocation.CreatedAt.Equal(created) {
		t.Fatal("recovery lost allocation identity or original creation time", allocation)
	}
	for _, prefix := range []string{"10.0.0.0/24", "fd00:8::/64", "fd00:44::11/128"} {
		if !slices.ContainsFunc(inventory.Reservations, func(r recovery.NetworkReservation) bool { return r.Prefix == prefix }) {
			t.Fatal("recovery lost a protected network range", prefix)
		}
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

func TestPausedAgentInventoriesWithoutAutomationAndAppliesOnlyApprovedCheckpoint(t *testing.T) {
	store := openTestLocalState(t)
	a := reconciliation.Authority{InstallationID: "installation", Generation: "generation", ClusterID: "cluster-a"}
	if _, err := store.admitAuthority(a); err != nil {
		t.Fatal(err)
	}
	state := testDesiredState(1, 1, "matching")
	state.InstallationId, state.RecoveryGeneration = a.InstallationID, a.Generation
	if _, err := store.acceptDesired(a.ClusterID, "test-session", state); err != nil {
		t.Fatal(err)
	}
	runtime := &supervisorTestRuntime{inventory: []RuntimeResource{{AllocationID: "matching", RuntimeID: "matching-runtime"}, {AllocationID: "unknown", RuntimeID: "unknown-runtime"}}}
	s := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	s.recoveryPaused = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, a.ClusterID); err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 0 {
		t.Fatal("paused restart applied cached desired state")
	}
	if _, err := s.AcceptDesired(a.ClusterID, "test-session", state); err == nil {
		t.Fatal("ordinary command authorized recovery reconciliation")
	}
	// Host administration enables checkpoints after approval, with cleanup still paused.
	cancel()
	s = newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	s.recoveryPaused, s.recoveryCheckpoints = true, true
	if _, err := s.AcceptDesired(a.ClusterID, "test-session", state); err != nil {
		t.Fatal(err)
	}
	s.reconcile(context.Background(), "desired-state")
	s.reconcile(context.Background(), "safety-resync")
	s.reconcile(context.Background(), "disk-enforcement")
	if runtime.calls != 1 || runtime.cleanup[0] {
		t.Fatal("approved checkpoint enabled background cleanup", runtime.calls, runtime.cleanup)
	}
	if _, err := s.AcceptDiff(a.ClusterID, "test-session", &agentv1.AllocationDiff{}); err == nil {
		t.Fatal("incremental updates resumed before verification")
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

func TestApprovedCheckpointReadinessPreservesQuarantine(t *testing.T) {
	store := openTestLocalState(t)
	authority := reconciliation.Authority{InstallationID: "installation", Generation: "recovery", ClusterID: "cluster-a", Paused: true, Checkpoints: true}
	if _, err := store.admitAuthority(authority); err != nil {
		t.Fatal(err)
	}
	runtime := &supervisorTestRuntime{inventory: []RuntimeResource{{AllocationID: "unknown", RuntimeID: "quarantined-runtime"}}}
	s := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
	s.recoveryPaused, s.recoveryCheckpoints = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx, authority.ClusterID); err != nil {
		t.Fatal(err)
	}
	app := &App{stateStore: store, supervisor: s, recoveryAuthority: authority}
	if report := app.readyReport(ctx); report.Status == health.StatusReady || report.Checkpoint.Complete {
		t.Fatal("admission alone established checkpoint readiness", report)
	}
	state := testDesiredState(1, 1)
	state.InstallationId, state.RecoveryGeneration = authority.InstallationID, authority.Generation
	if _, err := s.AcceptDesired(authority.ClusterID, "test-session", state); err != nil {
		t.Fatal(err)
	}
	if report := app.readyReport(ctx); report.Checkpoint.Complete {
		t.Fatal("checkpoint omitted credentials and replica acknowledgment")
	}
	deadline := timestamppb.New(time.Now().Add(15 * time.Second))
	creds := &agentv1.PullCredentialSet{AgentId: "node-1", ClusterId: authority.ClusterID, InstallationId: authority.InstallationID, RecoveryGeneration: authority.Generation, SessionId: "test-session", AuthorityEpoch: 1, AuthorityNotAfter: deadline}
	creds.CredentialsVersion = reconciliation.HashCredentials(nil)
	if _, err := s.AcceptCredentials(authority.ClusterID, "test-session", creds); err != nil {
		t.Fatal(err)
	}
	replicas := &agentv1.ReplicaEndpoints{AgentId: "node-1", ClusterId: authority.ClusterID, InstallationId: authority.InstallationID, RecoveryGeneration: authority.Generation, SessionId: "test-session", AuthorityEpoch: 1, AuthorityNotAfter: deadline, ReplicaAddresses: []string{"core:9443"}}
	replicas.ReplicasVersion = reconciliation.HashReplicas(replicas.ReplicaAddresses)
	if _, err := s.AcceptReplicas(authority.ClusterID, "test-session", replicas); err != nil {
		t.Fatal(err)
	}
	// Ordinary upgrade quiescence disables delivery after recovery, while
	// approval and the independently verified checkpoint remain valid.
	app.recoveryAuthority.Checkpoints, s.recoveryCheckpoints = false, false
	if _, err := store.admitAuthority(app.recoveryAuthority); err != nil {
		t.Fatal(err)
	}
	for _, paused := range []bool{true, false} {
		app.recoveryAuthority.Paused, s.recoveryPaused = paused, paused
		if report := app.readyReport(ctx); report.Status != health.StatusReady || !report.Checkpoint.Complete {
			t.Fatal("accepted approved checkpoint remained unavailable", report)
		}
		s.reconcile(ctx, "desired-state")
	}
	summary, err := store.summary()
	if err != nil || summary.Initialization != initializationRecovery || len(summary.RuntimeResources) != 1 || runtime.calls != 1 || runtime.cleanup[0] {
		t.Fatal("checkpoint readiness released quarantined resources or cleanup", summary, err)
	}
	app.recoveryAuthority.Generation = "other"
	if report := app.readyReport(ctx); report.Status == health.StatusReady || report.Checkpoint.Complete {
		t.Fatal("checkpoint was admitted under another authority", report)
	}
	if _, err := store.admitAuthority(app.recoveryAuthority); err != nil {
		t.Fatal(err)
	}
	if summary, err := store.summary(); err != nil || summary.CheckpointApproved {
		t.Fatal("approval survived a generation change", summary, err)
	}
}

func TestRecoveryPreservesHostProvisionedTLSAndDiscardsInterruptedOldCache(t *testing.T) {
	store := openTestLocalState(t)
	if _, err := store.admitAuthority(reconciliation.Authority{InstallationID: "installation", Generation: "before", ClusterID: "old"}); err != nil {
		t.Fatal(err)
	}
	material, err := identity.IssueClientCertificate(context.Background(), signkeystest.New(t), identity.CallerAgent, "node-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := activeCAIdentity(material.CAPEM)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "authority.json")
	authority := reconciliation.Authority{InstallationID: "installation", Generation: "recovery", ClusterID: cluster, Paused: true}
	encoded, _ := json.Marshal(authority)
	if err := os.WriteFile(file, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{stateStore: store, cfg: config.AgentConfig{AuthorityFile: file, Runtime: config.RuntimeConfig{DataDir: t.TempDir()}}}
	if err := app.persistClientTLSMaterial(material.KeyPEM, material.CertPEM, material.CAPEM); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(app.clientTLSDir(), "generation")
	if err := os.WriteFile(marker, []byte(authority.Generation), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.admitRecoveryAuthority(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ensureClientTLSMaterial(context.Background()); err != nil {
		t.Fatal("host-provisioned recovery credentials were deleted before paused startup", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := app.admitRecoveryAuthority(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.clientTLSDir(), agentCertFileName)); !os.IsNotExist(err) {
		t.Fatal("interrupted prior-generation cache survived admission", err)
	}
}
