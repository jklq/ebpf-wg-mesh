package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/grpc"
)

type Server struct {
	cfg          config.ControlPlaneConfig
	store        *Store
	logStore     *LogStore
	logEmitter   *LogEmitter
	notifier     *Notifier
	authority    *TLSAuthority
	internalGRPC *grpc.Server
	ingress      *IngressSyncer
	dashboard    *ManagedDashboardReconciler
	registry     *RegistryPolicy
	github       *GitHubCatalog
	webhooks     *GitHubWebhookProcessor
	coordinator  *GitHubCoordinator
	reconciler   *GitHubReconciler
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
	logStore, err := OpenLogStore(ctx, cfg.Logs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	logEmitter := NewLogEmitter(logStore)
	notifier := NewNotifier()
	ingressOpts := []IngressSyncerOption{
		WithIngressListenAddrs(cfg.Ingress.ListenAddrs),
		WithIngressAdminListen(cfg.Ingress.AdminListen),
		WithIngressAutomaticHTTPSDisabled(cfg.Ingress.DisableAutomaticHTTPS),
	}
	if len(cfg.Ingress.StaticRoutes) > 0 {
		staticRoutes := make([]IngressStaticRoute, 0, len(cfg.Ingress.StaticRoutes))
		for _, route := range cfg.Ingress.StaticRoutes {
			staticRoutes = append(staticRoutes, IngressStaticRoute{
				Hosts:    append([]string(nil), route.Hosts...),
				Upstream: route.Upstream,
			})
		}
		ingressOpts = append(ingressOpts, WithIngressStaticRoutes(staticRoutes))
	}
	ingress := NewIngressSyncer(cfg.Ingress.AdminURL, store, ingressOpts...)
	authority, err := NewTLSAuthority(cfg)
	if err != nil {
		return nil, err
	}
	internalCreds, err := authority.TransportCredentials()
	if err != nil {
		return nil, err
	}

	registry := NewRegistryPolicy(cfg.Registry)
	var githubClient *GitHubClient
	var githubCatalog *GitHubCatalog
	var githubCoordinator *GitHubCoordinator
	var webhookHandler *GitHubWebhookHandler
	var webhookProcessor *GitHubWebhookProcessor
	var githubReconciler *GitHubReconciler
	if cfg.GitHub.Enabled {
		githubClient, err = NewGitHubClient(cfg.GitHub)
		if err != nil {
			return nil, err
		}
		githubCatalog = NewGitHubCatalog(store, githubClient)
		githubCoordinator = NewGitHubCoordinator(store, githubCatalog, githubClient, 5*time.Minute, WithGitHubCoordinatorLogEmitter(logEmitter))
		githubReconciler = NewGitHubReconciler(store, githubCoordinator, time.Duration(cfg.Builder.HeartbeatTimeoutSeconds)*time.Second, 5*time.Minute, 5*time.Minute)
		webhookProcessor = NewGitHubWebhookProcessor(store, githubCoordinator)
		webhookHandler = NewGitHubWebhookHandler(store, cfg.GitHub.WebhookSecret, webhookProcessor)
	}

	platformService := NewPlatformService(
		store,
		notifier,
		ingress,
		WithServiceLogs(logStore),
		WithServiceLogEmitter(logEmitter),
		WithGitHubSourceInspection(githubCatalog, githubClient),
	)
	authz := NewInternalAuth()
	dashboard := NewManagedDashboardReconciler(cfg.Dashboard, cfg.Database, store, authority, ingress)
	internal := grpc.NewServer(
		grpc.Creds(internalCreds),
		grpc.UnaryInterceptor(authz.UnaryServerInterceptor()),
		grpc.StreamInterceptor(authz.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, NewAgentService(store, logStore, notifier, ingress, authority, dashboard))
	platformv1.RegisterPlatformServiceServer(internal, platformService)
	platformv1.RegisterBuilderServiceServer(internal, NewBuilderService(store, notifier, registry, time.Duration(cfg.Builder.HeartbeatTimeoutSeconds)*time.Second, WithBuilderLogEmitter(logEmitter)))
	platformv1.RegisterOpsServiceServer(internal, NewOpsService(webhookHandler))
	internalLn, err := net.Listen("tcp", cfg.InternalGRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen internal grpc: %w", err)
	}

	return &Server{
		cfg:          cfg,
		store:        store,
		logStore:     logStore,
		logEmitter:   logEmitter,
		notifier:     notifier,
		authority:    authority,
		internalGRPC: internal,
		ingress:      ingress,
		dashboard:    dashboard,
		registry:     registry,
		github:       githubCatalog,
		webhooks:     webhookProcessor,
		coordinator:  githubCoordinator,
		reconciler:   githubReconciler,
		internalLn:   internalLn,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	if s.ingress != nil {
		if err := s.ingress.Sync(ctx); err != nil {
			slog.Warn("initial ingress sync failed", "error", err)
		}
	}
	if s.reconciler != nil {
		if err := s.reconciler.Bootstrap(ctx); err != nil {
			slog.Warn("github bootstrap reconcile failed", "error", err)
		}
	}
	if s.dashboard != nil {
		if err := s.dashboard.Reconcile(ctx); err != nil && !errors.Is(err, errNoPlacementAvailable) {
			slog.Warn("initial managed dashboard reconcile failed", "error", err)
		}
	}
	errCh := make(chan error, 3)
	go func() {
		errCh <- serveGRPC(s.internalGRPC, s.internalLn)
	}()
	if s.webhooks != nil {
		go func() {
			errCh <- s.webhooks.Run(ctx)
		}()
	}
	if s.reconciler != nil {
		go func() {
			errCh <- s.reconciler.Run(ctx)
		}()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) Close() error {
	var errs []error
	if s.internalGRPC != nil {
		s.internalGRPC.GracefulStop()
	}
	if s.internalLn != nil {
		_ = s.internalLn.Close()
	}
	if s.store != nil {
		errs = append(errs, s.store.Close())
	}
	if s.logStore != nil {
		errs = append(errs, s.logStore.Close())
	}
	return errors.Join(errs...)
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

func (s *Server) EnsureBuilderClientIdentity(id string) (ClientIdentityMaterial, error) {
	if s == nil || s.authority == nil {
		return ClientIdentityMaterial{}, errors.New("controlplane authority is not initialized")
	}
	return s.authority.EnsureBuilderClientIdentity(id)
}

func (s *Server) HasHealthyAgent(ctx context.Context, agentID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("controlplane store is not initialized")
	}
	rec, err := s.store.agentByID(ctx, agentID)
	switch {
	case err == nil:
		return rec.LastSeenAt.After(time.Now().UTC().Add(-30 * time.Second)), nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

func serveGRPC(server *grpc.Server, ln net.Listener) error {
	if err := server.Serve(ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
