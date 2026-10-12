package controlplane

import (
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestDrainEndsStreamsAndRejectsNewOnes(t *testing.T) {
	drain := newReplicaDrain()
	server := grpc.NewServer(grpc.ChainStreamInterceptor(drain.streamInterceptor))
	healthpb.RegisterHealthServer(server, health.NewServer())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ln)
	defer server.Stop()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watch, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Recv(); err != nil {
		t.Fatal(err)
	}
	drain.begin()
	_, err = watch.Recv()
	if reason, _, ok := agentv1.ReconnectGuidance(err); status.Code(err) != codes.Unavailable || !ok || reason != agentv1.ReasonReplicaDraining {
		t.Fatalf("draining stream ended with %v, want draining guidance so the client resumes on a peer", err)
	}
	again, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		_, err = again.Recv()
	}
	if reason, _, ok := agentv1.ReconnectGuidance(err); status.Code(err) != codes.Unavailable || !ok || reason != agentv1.ReasonReplicaDraining {
		t.Fatalf("new stream on a draining replica returned %v", err)
	}
}

func TestDrainShedsManyStreamsOverTheirReconnectSpread(t *testing.T) {
	drain := newReplicaDrain()
	const streams = 500
	ended := make(chan time.Time, streams)
	started := make(chan struct{}, streams)
	for range streams {
		go func() {
			_ = drain.streamInterceptor(nil, fakeServerStream{ctx: context.Background()}, nil, func(_ any, ss grpc.ServerStream) error {
				started <- struct{}{}
				<-ss.Context().Done()
				ended <- time.Now()
				return ss.Context().Err()
			})
		}()
	}
	for range streams {
		<-started
	}
	begin := time.Now()
	<-drain.begin()
	var first, last time.Time
	for n := range streams {
		at := <-ended
		if n == 0 || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	spread := agentv1.ReconnectSpread(streams)
	if first.Sub(begin) > spread/4 || last.Sub(begin) < spread*3/4 {
		t.Fatalf("streams ended between %v and %v after drain began, want spread over %v", first.Sub(begin), last.Sub(begin), spread)
	}
	if agentv1.ReconnectSpread(3) > 20*time.Millisecond || agentv1.ReconnectSpread(1_000_000) != 10*time.Second {
		t.Fatal("small fleets must reconnect at once and large fleets within ten seconds")
	}
}

type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeServerStream) Context() context.Context { return s.ctx }

func TestDrainWithdrawsOnlyWhenPeersCanTakeTraffic(t *testing.T) {
	for _, replicas := range [][]string{{"a:9443"}, {"a:9443", "b:9443"}} {
		s := &Server{cfg: config.ControlPlaneConfig{ReplicaAddresses: replicas, Shutdown: config.ControlPlaneShutdownConfig{WithdrawalSeconds: 1, GraceSeconds: 5}}, drain: newReplicaDrain()}
		var handedOver time.Duration
		start := time.Now()
		s.drainTransports(context.Background(), func() { handedOver = time.Since(start) })
		elapsed := time.Since(start)
		if handedOver > 100*time.Millisecond {
			t.Fatalf("lease handed over %v into the drain, want before the withdrawal", handedOver)
		}
		if peers := len(replicas) > 1; peers != (elapsed >= time.Second) {
			t.Fatalf("%d replicas drained in %v", len(replicas), elapsed)
		}
	}
}

func TestDrainKeepsStreamsUntilOwnershipIsReleased(t *testing.T) {
	drain := newReplicaDrain()
	started, ended := make(chan struct{}), make(chan struct{})
	go func() {
		_ = drain.streamInterceptor(nil, fakeServerStream{ctx: context.Background()}, nil, func(_ any, ss grpc.ServerStream) error {
			close(started)
			<-ss.Context().Done()
			close(ended)
			return ss.Context().Err()
		})
	}()
	<-started
	s := &Server{cfg: config.ControlPlaneConfig{Shutdown: config.ControlPlaneShutdownConfig{GraceSeconds: 5}}, drain: drain}
	resigning, released, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		s.drainTransports(context.Background(), func() {
			close(resigning)
			<-released
		})
		close(stopped)
	}()
	<-resigning
	if !drain.active() {
		t.Fatal("ownership handover began before readiness withdrawal")
	}
	select {
	case <-ended:
		t.Fatal("stream ended before ownership was released")
	default:
	}
	close(released)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after ownership release")
	}
	<-stopped
}

func TestDrainTransportsFinishesAdmittedCallsAfterClosingListener(t *testing.T) {
	admitted, release := make(chan struct{}), make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	internal := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(admitted)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	go internal.Serve(ln)
	s := &Server{cfg: config.ControlPlaneConfig{Shutdown: config.ControlPlaneShutdownConfig{GraceSeconds: 10}}, drain: newReplicaDrain(), internalHTTP: internal}
	response := make(chan string, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		response <- string(body)
	}()
	<-admitted
	drained := make(chan struct{})
	go func() {
		s.drainTransports(context.Background(), nil)
		close(drained)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("draining replica still accepts connections")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if report := s.readyReport(context.Background()); !slices.Contains(report.Failed, "draining") {
		t.Fatalf("readiness during drain = %+v", report)
	}
	select {
	case <-drained:
		t.Fatal("drain returned before the admitted call finished")
	default:
	}
	close(release)
	if got := <-response; got != "finished" {
		t.Fatalf("admitted call returned %q", got)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after the admitted call completed")
	}
}
