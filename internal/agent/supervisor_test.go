package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/reconciliation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type slowSupervisorRuntime struct {
	supervisorTestRuntime
	started     chan struct{}
	interrupted chan struct{}
	blocked     atomic.Bool
}

func (r *slowSupervisorRuntime) ReconcileWithCleanup(ctx context.Context, state *agentv1.DesiredNodeState, cleanup bool) (*agentv1.StatusReport, error) {
	if r.blocked.CompareAndSwap(false, true) {
		close(r.started)
		<-ctx.Done()
		close(r.interrupted)
		return nil, ctx.Err()
	}
	return r.supervisorTestRuntime.ReconcileWithCleanup(ctx, state, cleanup)
}

func TestSupervisorAcceptsUpdatesDuringSlowStartupReconciliation(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		name := "stop allocation"
		if credentials {
			name = "refresh credentials"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := openTestLocalState(t)
			if err := store.prepareStartup("cluster-a", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := store.acceptDesired("cluster-a", "test-session", testDesiredState(1, 1, "alloc-1")); err != nil {
				t.Fatal(err)
			}
			runtime := &slowSupervisorRuntime{started: make(chan struct{}), interrupted: make(chan struct{})}
			s := newWorkloadSupervisor("node-1", runtime, store, func(context.Context, *agentv1.AssignedNodeConfig) error { return nil })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan error, 1)
			go func() { started <- s.Start(ctx, "cluster-a") }()
			select {
			case err := <-started:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("slow runtime blocked agent startup")
			}
			select {
			case <-runtime.started:
			case <-time.After(time.Second):
				t.Fatal("startup reconciliation did not run")
			}
			acceptedReplicas := make(chan error, 1)
			go func() {
				_, err := s.AcceptReplicas("cluster-a", "test-session", &agentv1.ReplicaEndpoints{
					AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session",
					AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)),
					ReplicasVersion: reconciliation.HashReplicas(nil),
				})
				acceptedReplicas <- err
			}()
			select {
			case err := <-acceptedReplicas:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("slow runtime blocked replica discovery update")
			}
			select {
			case <-runtime.interrupted:
				t.Fatal("replica discovery interrupted an image pull")
			default:
			}
			accepted := make(chan error, 1)
			go func() {
				var err error
				if credentials {
					update := &agentv1.PullCredentialSet{AgentId: "node-1", ClusterId: "cluster-a", SessionId: "test-session", AuthorityEpoch: 1, AuthorityNotAfter: timestamppb.New(time.Now().Add(15 * time.Second)), Credentials: []*agentv1.AllocationCredential{{AllocationId: "alloc-1", Username: "new-user", Password: "new-password"}}}
					update.CredentialsVersion = reconciliation.HashCredentials(update.Credentials)
					_, err = s.AcceptCredentials("cluster-a", "test-session", update)
				} else {
					_, err = s.AcceptDesired("cluster-a", "test-session", testDesiredState(1, 2))
				}
				accepted <- err
			}()
			select {
			case err := <-accepted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("slow runtime blocked control-plane update")
			}
			select {
			case <-runtime.interrupted:
			default:
				t.Fatal("update did not interrupt stale runtime work")
			}
			if report, err := s.CurrentReport(); err != nil || report != nil {
				t.Fatalf("cancelled reconciliation published a report: %v %v", report, err)
			}
			s.ReconcileAcceptedDesired()
			select {
			case <-s.ReportNotifications():
			case <-time.After(time.Second):
				t.Fatal("reconciliation did not resume with accepted state")
			}
			report, err := s.CurrentReport()
			if err != nil || report == nil {
				t.Fatalf("missing current report: %v", err)
			}
			if !credentials && (report.ReconciliationCursor != 2 || len(report.Services) != 0) {
				t.Fatalf("report retained stopped allocation: %v", report)
			}
		})
	}
}
