package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errStreamShed = errors.New("control-plane replica is draining")

type replicaDrain struct {
	draining atomic.Bool
	mu       sync.Mutex
	streams  map[*drainedStream]struct{}
	spread   time.Duration
	shed     chan struct{}
	shedOnce sync.Once
}

type drainedStream struct {
	cancel context.CancelCauseFunc
}

func newReplicaDrain() *replicaDrain {
	return &replicaDrain{streams: map[*drainedStream]struct{}{}, shed: make(chan struct{})}
}

func (d *replicaDrain) withdraw() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining.Swap(true) {
		return
	}
	d.spread = agentv1.ReconnectSpread(len(d.streams))
}

func (d *replicaDrain) begin() <-chan struct{} {
	d.withdraw()
	d.shedOnce.Do(func() {
		d.mu.Lock()
		streams := make([]*drainedStream, 0, len(d.streams))
		for stream := range d.streams {
			streams = append(streams, stream)
		}
		spread := d.spread
		d.mu.Unlock()
		rand.Shuffle(len(streams), func(i, j int) { streams[i], streams[j] = streams[j], streams[i] })
		go func() {
			defer close(d.shed)
			start := time.Now()
			for n, stream := range streams {
				if wait := time.Until(start.Add(spread * time.Duration(n) / time.Duration(len(streams)))); wait > 0 {
					time.Sleep(wait)
				}
				stream.cancel(errStreamShed)
			}
		}()
	})
	return d.shed
}

func (d *replicaDrain) active() bool { return d.draining.Load() }

func (d *replicaDrain) refusal() error {
	d.mu.Lock()
	spread := d.spread
	d.mu.Unlock()
	var after time.Duration
	if spread > 0 {
		after = rand.N(spread)
	}
	return agentv1.WithReconnectGuidance(status.Error(codes.Unavailable, errStreamShed.Error()), agentv1.ReasonReplicaDraining, after)
}

func (d *replicaDrain) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, cancel := context.WithCancelCause(ss.Context())
	defer cancel(nil)
	stream := &drainedStream{cancel: cancel}
	d.mu.Lock()
	if d.active() {
		d.mu.Unlock()
		return d.refusal()
	}
	d.streams[stream] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.streams, stream)
		d.mu.Unlock()
	}()
	err := handler(srv, drainingStream{ServerStream: ss, ctx: ctx})
	if errors.Is(context.Cause(ctx), errStreamShed) && ss.Context().Err() == nil {
		return agentv1.WithReconnectGuidance(status.Error(codes.Unavailable, errStreamShed.Error()), agentv1.ReasonReplicaDraining, 0)
	}
	return err
}

type drainingStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s drainingStream) Context() context.Context { return s.ctx }

func (s *Server) drainTransports(ctx context.Context, handover func()) {
	withdrawal := time.Duration(s.cfg.Shutdown.WithdrawalSeconds) * time.Second
	if len(s.cfg.ReplicaAddresses) < 2 {
		withdrawal = 0 // No balancer has another replica to withdraw this one to.
	}
	grace := time.Duration(s.cfg.Shutdown.GraceSeconds) * time.Second
	s.drain.withdraw()
	slog.Info("control-plane replica draining", "withdrawal", withdrawal, "grace", grace)
	if handover != nil {
		resigned := make(chan struct{})
		go func() {
			handover()
			close(resigned)
		}()
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-resigned:
		case <-timer.C:
			slog.Warn("singleton work did not stop within ten seconds; draining continues")
		case <-ctx.Done():
		}
		timer.Stop()
	}
	shed := s.drain.begin()
	if withdrawal > 0 {
		timer := time.NewTimer(withdrawal)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	select {
	case <-shed:
	case <-graceCtx.Done():
	}
	var wg sync.WaitGroup
	for _, server := range []*http.Server{s.internalHTTP, s.registryHTTP} {
		if server == nil {
			continue
		}
		wg.Go(func() {
			if err := server.Shutdown(graceCtx); err != nil {
				slog.Warn("control-plane calls did not finish within the drain grace period", "error", err)
			}
		})
	}
	if s.xdsGRPC != nil {
		wg.Go(func() {
			stopped := make(chan struct{})
			go func() {
				s.xdsGRPC.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-graceCtx.Done():
				s.xdsGRPC.Stop()
			}
		})
	}
	wg.Wait()
}
