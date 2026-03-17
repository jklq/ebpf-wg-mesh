package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc"
)

type Server struct {
	cfg          config.ControlPlaneConfig
	store        *Store
	notifier     *Notifier
	authority    *TLSAuthority
	publicHTTP   *http.Server
	internalGRPC *grpc.Server
	ingress      *IngressSyncer
	publicLn     net.Listener
	internalLn   net.Listener
}

func NewServer(ctx context.Context, cfg config.ControlPlaneConfig) (*Server, error) {
	store, err := OpenStore(cfg.Database, cfg.Mesh)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureBootstrap(ctx, cfg.Bootstrap); err != nil {
		return nil, err
	}
	notifier := NewNotifier()
	ingress := NewIngressSyncer(cfg.Ingress.AdminURL, cfg.Ingress.PublicAddr, cfg.Ingress.ControlPlaneHTTPUpstream, store)
	scheduler := NewScheduler(store)
	validator, err := NewValidator(cfg.OIDC)
	if err != nil {
		return nil, err
	}
	authority, err := NewTLSAuthority(cfg)
	if err != nil {
		return nil, err
	}
	internalCreds, err := authority.TransportCredentials()
	if err != nil {
		return nil, err
	}

	platformService := NewPlatformService(store, scheduler, notifier, ingress)
	publicHTTP := &http.Server{Handler: NewHTTPAPIHandler(validator, platformService)}
	internal := grpc.NewServer(grpc.Creds(internalCreds))
	agentv1.RegisterAgentControlServer(internal, NewAgentService(store, notifier, ingress, authority))

	publicLn, err := net.Listen("tcp", cfg.PublicHTTP.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen public http: %w", err)
	}
	internalLn, err := net.Listen("tcp", cfg.InternalGRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen internal grpc: %w", err)
	}

	return &Server{
		cfg:          cfg,
		store:        store,
		notifier:     notifier,
		authority:    authority,
		publicHTTP:   publicHTTP,
		internalGRPC: internal,
		ingress:      ingress,
		publicLn:     publicLn,
		internalLn:   internalLn,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	go func() {
		errCh <- serveHTTP(s.publicHTTP, s.publicLn)
	}()
	go func() {
		errCh <- serveGRPC(s.internalGRPC, s.internalLn)
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) Close() error {
	if s.publicHTTP != nil {
		_ = s.publicHTTP.Close()
	}
	if s.internalGRPC != nil {
		s.internalGRPC.GracefulStop()
	}
	if s.publicLn != nil {
		_ = s.publicLn.Close()
	}
	if s.internalLn != nil {
		_ = s.internalLn.Close()
	}
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func serveGRPC(server *grpc.Server, ln net.Listener) error {
	if err := server.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func serveHTTP(server *http.Server, ln net.Listener) error {
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
