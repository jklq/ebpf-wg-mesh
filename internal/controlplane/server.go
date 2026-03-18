package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
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
	dashboard    *ManagedDashboardReconciler
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
	ingress := NewIngressSyncer(cfg.Ingress.AdminURL, store)
	scheduler := NewScheduler(store)
	authority, err := NewTLSAuthority(cfg)
	if err != nil {
		return nil, err
	}
	internalCreds, err := authority.TransportCredentials()
	if err != nil {
		return nil, err
	}

	platformService := NewPlatformService(store, scheduler, notifier, ingress)
	authz := NewInternalAuth()
	dashboard := NewManagedDashboardReconciler(cfg.Dashboard, cfg.Database, store, authority, ingress)
	publicHTTP := &http.Server{Handler: NewOpsHandler()}
	internal := grpc.NewServer(
		grpc.Creds(internalCreds),
		grpc.UnaryInterceptor(authz.UnaryServerInterceptor()),
		grpc.StreamInterceptor(authz.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, NewAgentService(store, notifier, ingress, authority, dashboard))
	platformv1.RegisterPlatformServiceServer(internal, platformService)

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
		dashboard:    dashboard,
		publicLn:     publicLn,
		internalLn:   internalLn,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	if s.dashboard != nil {
		if err := s.dashboard.Reconcile(ctx); err != nil && !errors.Is(err, errNoPlacementAvailable) {
			slog.Warn("initial managed dashboard reconcile failed", "error", err)
		}
	}
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

func (s *Server) PublicAddr() string {
	if s == nil || s.publicLn == nil {
		return ""
	}
	return s.publicLn.Addr().String()
}

func (s *Server) InternalAddr() string {
	if s == nil || s.internalLn == nil {
		return ""
	}
	return s.internalLn.Addr().String()
}

func (s *Server) EnsureDashboardClientIdentity(id string) (ClientIdentityMaterial, error) {
	if s == nil || s.authority == nil {
		return ClientIdentityMaterial{}, errors.New("controlplane authority is not initialized")
	}
	return s.authority.EnsureDashboardClientIdentity(id)
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
