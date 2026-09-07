package controlplane

import (
	"context"
	"database/sql"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	platformv1connect "ebof-wg-mesh/api/proto/platformv1connect"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"

	"google.golang.org/grpc"
)

type Server struct {
	cfg             config.ControlPlaneConfig
	store           *Store
	delivery        *deliverycore.Delivery
	logStore        *LogStore
	logEmitter      *LogEmitter
	notifier        *Notifier
	authority       *TLSAuthority
	internalGRPC    *grpc.Server
	internalHTTP    *http.Server
	ingress         *IngressSyncer
	dashboard       *ManagedDashboardReconciler
	registry        *RegistryPolicy
	registryAuth    *RegistryAuth
	registryHTTP    *http.Server
	github          *GitHubCatalog
	webhooks        *GitHubWebhookProcessor
	coordinator     *GitHubCoordinator
	reconciler      *GitHubReconciler
	rollouts        *RolloutReconciler
	failover        *ServiceFailoverReconciler
	leases          *LeaseManager
	buildStaleAfter time.Duration
	internalLn      net.Listener
	registryLn      net.Listener
	healthShutdown  func(context.Context) error
	runMu           sync.Mutex
	runCancel       context.CancelFunc
	runDone         chan struct{}
	runStarted      bool
	closed          bool
}

func NewServer(ctx context.Context, cfg config.ControlPlaneConfig) (*Server, error) {
	store, err := OpenStore(cfg.Database, cfg.Mesh)
	if err != nil {
		return nil, err
	}
	leases := NewLeaseManager(store, 15*time.Second, time.Second)
	store.useReportedAllocationIP = cfg.Ingress.UseReportedAllocationIP
	archiveStore, err := NewFileSourceArchiveStore(cfg.SourceArchives.Directory)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := verifySharedControlPlaneDirectory(ctx, store, "state", cfg.StateDir); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := verifySharedControlPlaneDirectory(ctx, store, "source-archives", cfg.SourceArchives.Directory); err != nil {
		_ = store.Close()
		return nil, err
	}
	initializationCtx, releaseInitialization, err := leases.hold(ctx, "control-plane-initialization")
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	defer releaseInitialization()
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
	var authority *TLSAuthority
	var registryAuth *RegistryAuth
	if err := store.withLeaseGuard(initializationCtx, func() error {
		var err error
		authority, err = NewTLSAuthority(cfg)
		if err != nil {
			return err
		}
		registryAuth, err = NewRegistryAuth(cfg.Registry, cfg.StateDir)
		if err != nil {
			return fmt.Errorf("initialize embedded registry auth: %w", err)
		}
		return nil
	}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("initialize shared control-plane identity: %w", err)
	}
	logStore, err := OpenLogStore(ctx, cfg.Logs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	logEmitter := NewLogEmitter(logStore)
	notifier := NewNotifier(ctx, store, 0)
	platformEvents := NewPlatformEvents(store, 0)
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
	registry := NewRegistryPolicy(cfg.Registry, registryAuth)
	delivery := newDelivery(store, notifier, ingress, platformEvents)
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
		githubCoordinator = NewGitHubCoordinator(store, delivery, githubCatalog, githubClient, 5*time.Minute, WithGitHubCoordinatorLogEmitter(logEmitter))
		githubReconciler = NewGitHubReconciler(store, githubCoordinator, time.Duration(cfg.Builder.HeartbeatTimeoutSeconds)*time.Second, 5*time.Minute, 5*time.Minute)
		webhookProcessor = NewGitHubWebhookProcessor(store, githubCoordinator)
		webhookHandler = NewGitHubWebhookHandler(store, cfg.GitHub.WebhookSecret, webhookProcessor)
	}

	platformService := NewPlatformService(
		store,
		notifier,
		ingress,
		delivery,
		WithServiceLogs(logStore),
		WithServiceLogEmitter(logEmitter),
		WithGitHubSourceInspection(githubCatalog, githubClient),
		WithPlatformDomainSuffix(cfg.Ingress.PublicAddr),
		WithPlatformEvents(platformEvents),
	)
	authz := NewInternalAuth(cfg.Dashboard.ServiceCallerID, cfg.UserAssertions.HMACSecret, authority.revocations)
	dashboard := NewManagedDashboardReconciler(cfg.Dashboard, cfg.Profile, store, delivery, ingress, notifier)
	rollouts := NewRolloutReconciler(delivery, 2*time.Second)
	failover := NewServiceFailoverReconciler(delivery,
		time.Duration(cfg.Failover.ReconcileIntervalSeconds)*time.Second,
		time.Duration(cfg.Failover.UnhealthyThresholdSeconds)*time.Second)
	internal := grpc.NewServer(
		grpc.UnaryInterceptor(authz.UnaryServerInterceptor()),
		grpc.StreamInterceptor(authz.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, NewAgentService(
		store, delivery, logStore, notifier, authority, dashboard,
		cfg.Dashboard.Enabled, cfg.Dashboard.TrustedAgentID, cfg.Dashboard.ServiceCallerID,
		WithAgentRegistry(registry),
	))
	platformv1.RegisterPlatformServiceServer(internal, platformService)
	buildOperations := NewBuildOperations(store, delivery, registry, registry, time.Duration(cfg.Builder.HeartbeatTimeoutSeconds)*time.Second, WithBuilderLogEmitter(logEmitter))
	platformv1.RegisterBuilderServiceServer(internal, NewBuilderService(buildOperations))
	opsService := NewOpsService(webhookHandler, store, delivery, notifier, authority)
	platformv1.RegisterOpsServiceServer(internal, opsService)
	internalLn, err := net.Listen("tcp", cfg.InternalGRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen internal grpc: %w", err)
	}
	internalHTTP := &http.Server{
		Handler:   dualProtocolHandler(internal, newConnectHandler(authz, connectPlatformService{platformService}, connectOpsService{opsService})),
		TLSConfig: authority.HTTPConfig(),
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
		cfg:             cfg,
		store:           store,
		delivery:        delivery,
		logStore:        logStore,
		logEmitter:      logEmitter,
		notifier:        notifier,
		authority:       authority,
		internalGRPC:    internal,
		ingress:         ingress,
		dashboard:       dashboard,
		registry:        registry,
		registryAuth:    registryAuth,
		registryHTTP:    registryHTTP,
		github:          githubCatalog,
		webhooks:        webhookProcessor,
		coordinator:     githubCoordinator,
		reconciler:      githubReconciler,
		rollouts:        rollouts,
		failover:        failover,
		leases:          leases,
		buildStaleAfter: time.Duration(cfg.Builder.HeartbeatTimeoutSeconds) * time.Second,
		internalLn:      internalLn,
		internalHTTP:    internalHTTP,
		registryLn:      registryLn,
		runDone:         make(chan struct{}),
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
	runCtx, cancel := context.WithCancel(ctx)
	s.runMu.Lock()
	if s.closed {
		s.runMu.Unlock()
		cancel()
		return errors.New("control-plane server is closed")
	}
	if s.runStarted {
		s.runMu.Unlock()
		cancel()
		return errors.New("control-plane server is already running")
	}
	s.runStarted = true
	s.runCancel = cancel
	s.runMu.Unlock()
	defer func() {
		cancel()
		close(s.runDone)
	}()

	errCh := make(chan error, 8)
	go func() {
		errCh <- serveInternalHTTP(s.internalHTTP, s.internalLn)
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
	if s.reconciler != nil {
		go func() {
			errCh <- s.reconciler.Run(runCtx)
		}()
	}
	leaseDone := make(chan error, 1)
	go func() {
		err := s.leases.Run(runCtx, "control-plane-singleton", s.runSingletonJobs)
		leaseDone <- err
		errCh <- err
	}()
	var result error
	select {
	case <-runCtx.Done():
	case result = <-errCh:
	}
	cancel()
	leaseErr := <-leaseDone
	if result == nil {
		result = leaseErr
	}
	return result
}

func (s *Server) runSingletonJobs(ctx context.Context) error {
	if s.reconciler != nil {
		if err := s.reconciler.Bootstrap(ctx); err != nil {
			slog.Warn("github bootstrap reconcile failed", "error", err)
		}
	}
	errCh := make(chan error, 7)
	if s.webhooks != nil {
		go func() { errCh <- s.webhooks.Run(ctx) }()
	}
	if s.ingress != nil {
		go func() { errCh <- s.ingress.Run(ctx) }()
	}
	if s.rollouts != nil {
		go func() { errCh <- s.rollouts.Run(ctx) }()
	}
	if s.failover != nil {
		go func() { errCh <- s.failover.Run(ctx) }()
	}
	if s.dashboard != nil {
		go func() { errCh <- s.dashboard.Run(ctx) }()
	}
	go func() { errCh <- s.buildLeaseRepairLoop(ctx) }()
	go func() { s.sourceArchiveRetentionLoop(ctx); errCh <- nil }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) buildLeaseRepairLoop(ctx context.Context) error {
	interval := s.buildStaleAfter / 3
	if interval < time.Second {
		interval = time.Second
	}
	repair := func() {
		if err := s.delivery.RecoverExpiredBuilds(ctx, s.buildStaleAfter); err != nil && ctx.Err() == nil {
			slog.Warn("expired build lease repair failed", "error", err)
		}
	}
	repair()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			repair()
		}
	}
}

func (s *Server) sourceArchiveRetentionLoop(ctx context.Context) {
	prune := func() {
		now, err := deliverycore.DatabaseTime(ctx, s.store.db)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("source archive retention database time failed", "error", err)
			}
			return
		}
		cutoff := now.AddDate(0, 0, -s.cfg.SourceArchives.RetentionDays)
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
	s.runMu.Lock()
	s.closed = true
	runCancel := s.runCancel
	runDone := s.runDone
	runStarted := s.runStarted
	s.runMu.Unlock()
	if runCancel != nil {
		runCancel()
	}
	if runStarted {
		<-runDone
	}
	if s.healthShutdown != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.healthShutdown(shutdownCtx)
		cancel()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if s.internalLn != nil {
		_ = s.internalLn.Close()
	}
	if s.internalGRPC != nil {
		s.internalGRPC.GracefulStop()
	}
	if s.internalHTTP != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.internalHTTP.Shutdown(shutdownCtx)
		cancel()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
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
	rec, err := s.store.deliveryQueries().AgentByID(ctx, agentID)
	switch {
	case err == nil:
		now, err := deliverycore.DatabaseTime(ctx, s.store.db)
		if err != nil {
			return false, err
		}
		return rec.LastSeenAt.After(now.Add(-deliverycore.AgentHealthyTTL)), nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

func serveInternalHTTP(server *http.Server, ln net.Listener) error {
	if err := server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newConnectHandler exposes PlatformService and OpsService over the Connect
// protocol, enforcing the same authorization rules as the gRPC interceptors.
func newConnectHandler(authz *InternalAuth, platformService platformv1connect.PlatformServiceHandler, opsService platformv1connect.OpsServiceHandler) http.Handler {
	options := []connect.HandlerOption{
		connect.WithInterceptors(authz.ConnectInterceptor()),
	}
	mux := http.NewServeMux()
	mux.Handle(platformv1connect.NewPlatformServiceHandler(platformService, options...))
	mux.Handle(platformv1connect.NewOpsServiceHandler(opsService, options...))
	return mux
}

// dualProtocolHandler routes gRPC traffic (HTTP/2 with an application/grpc
// content type) to the gRPC server and everything else to the Connect
// handlers. Both protocols share one TLS listener; the verified client
// certificate is extracted once and carried into both request contexts.
func dualProtocolHandler(grpcServer *grpc.Server, connectHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
			ctx = context.WithValue(ctx, verifiedClientCertificateContextKey{}, r.TLS.VerifiedChains[0][0])
		}
		r = r.WithContext(ctx)
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		connectHandler.ServeHTTP(w, r)
	})
}
