package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"

	"google.golang.org/grpc"
)

type Server struct {
	cfg            config.ControlPlaneConfig
	store          *Store
	logStore       *LogStore
	logEmitter     *LogEmitter
	notifier       *Notifier
	authority      *TLSAuthority
	internalGRPC   *grpc.Server
	ingress        *IngressSyncer
	dashboard      *ManagedDashboardReconciler
	registry       *RegistryPolicy
	registryAuth   *RegistryAuth
	registryHTTP   *http.Server
	github         *GitHubCatalog
	webhooks       *GitHubWebhookProcessor
	coordinator    *GitHubCoordinator
	reconciler     *GitHubReconciler
	rollouts       *RolloutReconciler
	expiry         *AgentExpiryTracker
	internalLn     net.Listener
	registryLn     net.Listener
	healthShutdown func(context.Context) error
}

func NewServer(ctx context.Context, cfg config.ControlPlaneConfig) (*Server, error) {
	store, err := OpenStore(cfg.Database, cfg.Mesh)
	if err != nil {
		return nil, err
	}
	store.useReportedAllocationIP = cfg.Ingress.UseReportedAllocationIP
	archiveStore, err := NewFileSourceArchiveStore(cfg.SourceArchives.Directory)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	store.ConfigureSourceArchives(archiveStore)
	if err := store.EnsureBootstrap(ctx, cfg.Bootstrap); err != nil {
		_ = store.Close()
		return nil, err
	}
	store.reserveAgents(cfg.Dashboard.TrustedAgentID)
	if err := store.ensureAgentBootstrapTokens(ctx, cfg.InternalGRPC.TLS.BootstrapTokens); err != nil {
		_ = store.Close()
		return nil, err
	}
	logStore, err := OpenLogStore(ctx, cfg.Logs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	logEmitter := NewLogEmitter(logStore)
	notifier := NewNotifier()
	platformEvents := NewPlatformEvents()
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
	expiry := NewAgentExpiryTracker(ctx, agentHealthyTTL, func(ctx context.Context, agentID string, cutoff time.Time) error {
		notifyAgentIDs, changedEnvironmentIDs, err := store.failoverServicesFromAgent(ctx, agentID, cutoff)
		if err != nil {
			return err
		}
		if len(notifyAgentIDs) > 0 {
			notifier.NotifyAll(notifyAgentIDs)
		}
		if len(notifyAgentIDs) > 0 || len(changedEnvironmentIDs) > 0 {
			ingress.RequestSync()
		}
		for _, environmentID := range changedEnvironmentIDs {
			platformEvents.Publish(environmentID)
		}
		return nil
	})
	authority, err := NewTLSAuthority(cfg)
	if err != nil {
		return nil, err
	}
	internalCreds, err := authority.TransportCredentials()
	if err != nil {
		return nil, err
	}

	registryAuth, err := NewRegistryAuth(cfg.Registry, cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("initialize embedded registry auth: %w", err)
	}
	registry := NewRegistryPolicy(cfg.Registry, registryAuth)
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
		githubCoordinator = NewGitHubCoordinator(store, githubCatalog, githubClient, 5*time.Minute, WithGitHubCoordinatorLogEmitter(logEmitter), WithGitHubCoordinatorPlatformEvents(platformEvents))
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
		WithPlatformDomainSuffix(cfg.Ingress.PublicAddr),
		WithPlatformEvents(platformEvents),
	)
	authz := NewInternalAuth(cfg.Dashboard.ServiceCallerID, cfg.UserAssertions.HMACSecret, authority.revocations)
	dashboard := NewManagedDashboardReconciler(cfg.Dashboard, cfg.Profile, store, ingress, notifier)
	rollouts := NewRolloutReconciler(store, notifier, ingress, platformEvents, 2*time.Second)
	internal := grpc.NewServer(
		grpc.Creds(internalCreds),
		grpc.UnaryInterceptor(authz.UnaryServerInterceptor()),
		grpc.StreamInterceptor(authz.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, NewAgentService(
		store, logStore, notifier, ingress, authority, dashboard,
		cfg.Dashboard.Enabled, cfg.Dashboard.TrustedAgentID, cfg.Dashboard.ServiceCallerID,
		WithAgentExpiryTracker(expiry), WithAgentPlatformEvents(platformEvents), WithAgentRegistry(registry),
	))
	platformv1.RegisterPlatformServiceServer(internal, platformService)
	platformv1.RegisterBuilderServiceServer(internal, NewBuilderService(store, notifier, registry, time.Duration(cfg.Builder.HeartbeatTimeoutSeconds)*time.Second, WithBuilderLogEmitter(logEmitter), WithBuilderPlatformEvents(platformEvents)))
	platformv1.RegisterOpsServiceServer(internal, NewOpsService(webhookHandler, store, notifier, authority))
	internalLn, err := net.Listen("tcp", cfg.InternalGRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen internal grpc: %w", err)
	}
	var registryLn net.Listener
	var registryHTTP *http.Server
	if registryAuth != nil {
		registryLn, err = net.Listen("tcp", cfg.Registry.AuthListen)
		if err != nil {
			_ = internalLn.Close()
			return nil, fmt.Errorf("listen registry auth: %w", err)
		}
		registryHTTP = &http.Server{
			Handler:           registryAuth,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       time.Minute,
		}
	}

	server := &Server{
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
		registryAuth: registryAuth,
		registryHTTP: registryHTTP,
		github:       githubCatalog,
		webhooks:     webhookProcessor,
		coordinator:  githubCoordinator,
		reconciler:   githubReconciler,
		rollouts:     rollouts,
		expiry:       expiry,
		internalLn:   internalLn,
		registryLn:   registryLn,
	}
	if listen := strings.TrimSpace(cfg.Health.Listen); listen != "" {
		_, shutdown, err := health.ListenAndServe(ctx, listen, server.readyReport)
		if err != nil {
			_ = internalLn.Close()
			if registryLn != nil {
				_ = registryLn.Close()
			}
			return nil, fmt.Errorf("listen health: %w", err)
		}
		server.healthShutdown = shutdown
	}
	return server, nil
}

func (s *Server) readyReport(ctx context.Context) health.Report {
	var failed []string
	databaseOK, migrationsOK := false, false
	if s != nil && s.store != nil {
		databaseOK, migrationsOK = s.store.Ready(ctx)
	}
	if !databaseOK {
		failed = append(failed, "database")
	}
	if !migrationsOK {
		failed = append(failed, "migrations")
	}
	if s != nil && s.logStore != nil && !s.logStore.Ready(ctx) {
		failed = append(failed, "logs")
	}
	if archive, ok := s.sourceArchiveStore(); !ok || !archive.Ready() {
		failed = append(failed, "source_storage")
	}
	if len(failed) > 0 {
		return health.Report{Status: health.StatusNotReady, Failed: failed}
	}
	return health.Report{Status: health.StatusReady}
}

func (s *Server) sourceArchiveStore() (*FileSourceArchiveStore, bool) {
	if s == nil || s.store == nil {
		return nil, false
	}
	archive, ok := s.store.sourceArchives.(*FileSourceArchiveStore)
	return archive, ok
}

func (s *Server) Run(ctx context.Context) error {
	if err := s.restoreAgentExpiryDeadlines(ctx, time.Now().UTC()); err != nil {
		return fmt.Errorf("restore agent expiry deadlines: %w", err)
	}
	go s.sourceArchiveRetentionLoop(ctx)
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
	errCh := make(chan error, 5)
	go func() {
		errCh <- serveGRPC(s.internalGRPC, s.internalLn)
	}()
	if s.registryHTTP != nil && s.registryLn != nil {
		go func() {
			err := s.registryHTTP.Serve(s.registryLn)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- err
		}()
	}
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
	if s.rollouts != nil {
		go func() {
			errCh <- s.rollouts.Run(ctx)
		}()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) restoreAgentExpiryDeadlines(ctx context.Context, now time.Time) error {
	if s == nil || s.expiry == nil || s.store == nil {
		return nil
	}
	agents, err := s.store.listAgents(ctx)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		s.expiry.Restore(agent.ID, agent.LastSeenAt, now)
	}
	return nil
}

func (s *Server) sourceArchiveRetentionLoop(ctx context.Context) {
	prune := func() {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.cfg.SourceArchives.RetentionDays)
		deleted, err := s.store.pruneSourceArchives(ctx, cutoff)
		if err != nil && ctx.Err() == nil {
			slog.Warn("source archive retention failed", "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("source archive retention completed", "objects_deleted", deleted)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

func (s *Server) Close() error {
	var errs []error
	if s.healthShutdown != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.healthShutdown(shutdownCtx)
		cancel()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if s.expiry != nil {
		s.expiry.Close()
	}
	if s.internalGRPC != nil {
		s.internalGRPC.GracefulStop()
	}
	if s.internalLn != nil {
		_ = s.internalLn.Close()
	}
	if s.registryHTTP != nil {
		err := s.registryHTTP.Close()
		if !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if s.registryLn != nil {
		_ = s.registryLn.Close()
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

func (s *Server) RegistryAuthAddr() string {
	if s == nil || s.registryLn == nil {
		return ""
	}
	return s.registryLn.Addr().String()
}

func (s *Server) RegistryAuthCertificatePath() string {
	if s == nil || s.registryAuth == nil {
		return ""
	}
	return s.registryAuth.CertificatePath()
}

func (s *Server) EnsureDashboardClientIdentity(id string) (ClientIdentityMaterial, error) {
	if s == nil || s.authority == nil {
		return ClientIdentityMaterial{}, errors.New("controlplane authority is not initialized")
	}
	if allowedID := s.cfg.Dashboard.ServiceCallerID; allowedID == "" || id != allowedID {
		return ClientIdentityMaterial{}, fmt.Errorf("dashboard client identity %q is not allowed", id)
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
		return rec.LastSeenAt.After(time.Now().UTC().Add(-agentHealthyTTL)), nil
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
