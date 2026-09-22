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
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/source"
	"ebof-wg-mesh/internal/controlplane/xds"

	"connectrpc.com/connect"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	platformv1connect "ebof-wg-mesh/api/proto/platformv1connect"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/health"

	"google.golang.org/grpc"
)

// publicationFence is held by the lease owner, never by product readers.
type publicationFence interface {
	SetPublishing(bool)
}

type Server struct {
	cfg             config.ControlPlaneConfig
	store           *persistence
	signKeys        *signkeys.Service
	signingScopes   []string
	delivery        *deliverycore.Delivery
	logStore        *logs.LogStore
	logEmitter      *logs.LogEmitter
	logIngester     *logs.AsyncIngester
	notifier        *Notifier
	authority       *identity.TLSAuthority
	internalGRPC    *grpc.Server
	internalHTTP    *http.Server
	ingress         *xds.Publisher
	xdsServer       *xds.Server
	xdsGRPC         *grpc.Server
	xdsLn           net.Listener
	dashboard       *ManagedDashboardReconciler
	registry        *registry.Policy
	registryAuth    *registry.Auth
	registryHTTP    *http.Server
	github          *source.GitHubCatalog
	webhooks        *source.GitHubWebhookProcessor
	coordinator     *source.GitHubCoordinator
	reconciler      *source.GitHubReconciler
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
	store, err := openPersistence(cfg.Database, cfg.Mesh,
		WithDeletionGracePeriod(time.Duration(cfg.Deletion.GracePeriodDays)*24*time.Hour))
	if err != nil {
		return nil, err
	}
	secrets, err := secretkeys.Open(ctx, store.db, cfg.SecretKeys, secretkeys.Options{
		// Production never generates missing keys: the keyring file is
		// explicitly provisioned and Open fails closed without it.
		AllowGenerate: !cfg.Profile.IsProduction(),
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open sealed secrets: %w", err)
	}
	store.attachSecrets(secrets)
	signKeys, err := signkeys.Open(ctx, store.db, secrets, signkeys.Options{
		// Production never generates missing keys: each scope is
		// initialized explicitly via the signing-keys CLI, and Open fails
		// closed without an active key.
		AllowGenerate:   !cfg.Profile.IsProduction(),
		RegistryEnabled: strings.TrimSpace(cfg.Registry.Host) != "",
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open signing keys: %w", err)
	}
	leases := NewLeaseManager(store.database, 15*time.Second, time.Second)
	leases.SetAdvertise(cfg.AdvertiseAddr)
	archiveStore, err := source.NewSourceArchiveStore(cfg.SourceArchives)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := verifySharedControlPlaneDirectory(ctx, store, "state", cfg.StateDir); err != nil {
		_ = store.Close()
		return nil, err
	}
	if cfg.SourceArchives.Provider == config.SourceArchiveProviderFile {
		if err := verifySharedControlPlaneDirectory(ctx, store, "source-archives", cfg.SourceArchives.Directory); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	initializationCtx, releaseInitialization, err := leases.hold(ctx, "control-plane-initialization")
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	defer releaseInitialization()
	store.source.ConfigureSourceArchives(archiveStore)
	if err := store.catalog.EnsureBootstrap(ctx, cfg.Bootstrap); err != nil {
		_ = store.Close()
		return nil, err
	}
	store.reserveAgents(cfg.Dashboard.TrustedAgentID)
	if err := store.fleet.ensureAgentBootstrapTokens(ctx, cfg.InternalGRPC.TLS.BootstrapTokens); err != nil {
		_ = store.Close()
		return nil, err
	}
	var authority *identity.TLSAuthority
	var registryAuth *registry.Auth
	if err := store.withLeaseGuard(initializationCtx, func() error {
		var err error
		authority, err = identity.NewTLSAuthority(initializationCtx, cfg, signKeys)
		if err != nil {
			return err
		}
		registryAuth, err = registry.NewAuth(initializationCtx, cfg.Registry, signKeys, cfg.StateDir)
		if err != nil {
			return fmt.Errorf("initialize embedded registry auth: %w", err)
		}
		return nil
	}); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("initialize shared control-plane identity: %w", err)
	}
	logStore, err := logs.OpenLogStore(ctx, cfg.Logs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if logStore != nil {
		logStore.SetProjectResolver(store.catalog.resolveLogRetention)
	}
	logEmitter := logs.NewLogEmitter(logStore)
	logIngester := logs.NewAsyncIngester(logStore, logs.AsyncIngesterConfig{
		QueueFlushes: cfg.Logs.IngestQueueFlushes,
		RatePerSec:   float64(cfg.Logs.IngestRatePerSec),
		Burst:        cfg.Logs.IngestBurst,
	})
	notifier := NewNotifier(store.notifications)
	platformEvents := NewPlatformEvents(store.events, 0)
	staticRoutes := make([]xds.StaticRoute, 0, len(cfg.Ingress.StaticRoutes))
	for _, route := range cfg.Ingress.StaticRoutes {
		staticRoutes = append(staticRoutes, xds.StaticRoute{
			Hosts:    append([]string(nil), route.Hosts...),
			Upstream: route.Upstream,
		})
	}
	xdsServer := xds.NewServer(context.Background())
	xdsServer.SetNodeStore(store.routing)
	publisherID := strings.TrimSpace(cfg.AdvertiseAddr)
	if publisherID == "" {
		publisherID = strings.TrimSpace(cfg.InternalGRPC.Listen)
	}
	ingress := xds.NewPublisher(xds.PublisherConfig{
		Source:       store.routing,
		Publications: store.routing,
		Nodes:        store.routing,
		Server:       xdsServer,
		Static:       staticRoutes,
		ListenAddrs:  cfg.Ingress.ListenAddrs,
		PublisherID:  publisherID,
	})
	xdsServer.SetFirstContactHook(ingress.Refresh)
	policy := registry.NewPolicy(cfg.Registry, registryAuth)
	scheduler := buildSchedulerConfigFromControlPlane(cfg.Builder)
	delivery := newDeliveryWithScheduler(store, &scheduler, notifier, ingress, platformEvents, logEmitter)
	var githubClient *source.GitHubClient
	var githubCatalog *source.GitHubCatalog
	var githubCoordinator *source.GitHubCoordinator
	var webhookHandler *source.GitHubWebhookHandler
	var webhookProcessor *source.GitHubWebhookProcessor
	var githubReconciler *source.GitHubReconciler
	if cfg.GitHub.Enabled {
		githubClient, err = source.NewGitHubClient(cfg.GitHub)
		if err != nil {
			return nil, err
		}
		githubCatalog = source.NewGitHubCatalog(store.source, githubClient)
		githubCoordinator = source.NewGitHubCoordinator(store.source, store.source.Work(), delivery, githubCatalog, githubClient, 5*time.Minute)
		githubReconciler = source.NewGitHubReconciler(store.source, githubCoordinator, 5*time.Minute)
		webhookProcessor = source.NewGitHubWebhookProcessor(store.source, githubCoordinator)
		webhookHandler = source.NewGitHubWebhookHandler(store.source, cfg.GitHub.WebhookSecret, webhookProcessor)
	}

	platformService := NewPlatformService(
		store.platform(),
		notifier,
		ingress,
		delivery,
		WithServiceLogs(logStore),
		WithServiceLogEmitter(logEmitter),
		WithGitHubSourceInspection(githubCatalog, githubClient, store.authorizer()),
		WithPlatformDomainSuffix(cfg.Ingress.PublicAddr),
		WithPlatformEvents(platformEvents),
		WithPlatformLiveOwner(leaseLiveOwner{leases: leases, name: SingletonLeaseName}),
	)
	internalAuth := identity.NewInternalAuth(cfg.Dashboard.ServiceCallerID, userAssertionSecrets(signKeys), authority.Revocations())
	dashboard := NewManagedDashboardReconciler(cfg.Dashboard, cfg.Profile, store.catalog, delivery, ingress, notifier)
	rollouts := NewRolloutReconciler(delivery, 2*time.Second)
	failover := NewServiceFailoverReconciler(delivery,
		time.Duration(cfg.Failover.ReconcileIntervalSeconds)*time.Second,
		time.Duration(cfg.Failover.UnhealthyThresholdSeconds)*time.Second)
	internal := grpc.NewServer(
		grpc.UnaryInterceptor(internalAuth.UnaryServerInterceptor()),
		grpc.StreamInterceptor(internalAuth.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, NewAgentService(
		store.fleet, delivery, logStore, notifier, authority, dashboard,
		cfg.Dashboard.Enabled, cfg.Dashboard.TrustedAgentID, cfg.Dashboard.ServiceCallerID,
		WithAgentRegistry(policy),
		WithReplicaAddresses(cfg.ReplicaAddresses),
		WithLiveOwner(leaseLiveOwner{leases: leases, name: SingletonLeaseName}),
		WithLogIngester(logIngester),
	))
	platformv1.RegisterPlatformServiceServer(internal, platformService)
	buildOperations := NewBuildOperations(store.builds, store.reads, store.source, delivery, policy, policy, WithBuilderLogEmitter(logEmitter))
	platformv1.RegisterBuilderServiceServer(internal, NewBuilderService(buildOperations))
	opsService := NewOpsService(webhookHandler, store.fleet, delivery, notifier, authority)
	platformv1.RegisterOpsServiceServer(internal, opsService)
	internalLn, err := net.Listen("tcp", cfg.InternalGRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen internal grpc: %w", err)
	}
	internalHTTP := &http.Server{
		Handler:   dualProtocolHandler(internal, newConnectHandler(internalAuth, connectPlatformService{platformService}, connectOpsService{opsService})),
		TLSConfig: authority.HTTPConfig(),
	}
	xdsLn, err := net.Listen("tcp", cfg.Ingress.XDSListen)
	if err != nil {
		_ = internalLn.Close()
		return nil, fmt.Errorf("listen xds: %w", err)
	}
	xdsGRPC := xdsServer.GRPCServer()
	var registryLn net.Listener
	var registryHTTP *http.Server
	if registryAuth != nil {
		registryLn, err = net.Listen("tcp", cfg.Registry.AuthListen)
		if err != nil {
			_ = xdsLn.Close()
			_ = internalLn.Close()
			return nil, fmt.Errorf("listen registry auth: %w", err)
		}
		registryHTTP = &http.Server{
			Handler:           registryAuth,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       time.Minute,
		}
	}

	requiredSigningScopes := []string{signkeys.ScopeInternalCA, signkeys.ScopeUserAssertion}
	if registryAuth != nil {
		requiredSigningScopes = append(requiredSigningScopes, signkeys.ScopeRegistry)
	}
	server := &Server{
		cfg:             cfg,
		store:           store,
		signKeys:        signKeys,
		signingScopes:   requiredSigningScopes,
		delivery:        delivery,
		logStore:        logStore,
		logEmitter:      logEmitter,
		logIngester:     logIngester,
		notifier:        notifier,
		authority:       authority,
		internalGRPC:    internal,
		ingress:         ingress,
		xdsServer:       xdsServer,
		xdsGRPC:         xdsGRPC,
		xdsLn:           xdsLn,
		dashboard:       dashboard,
		registry:        policy,
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
			_ = xdsLn.Close()
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
	if s == nil || s.store == nil || !s.store.source.SourceStorageReady() {
		failed = append(failed, "source_storage")
	}
	if s == nil || s.store == nil || s.store.secrets == nil || !s.store.secrets.Ready(ctx) {
		failed = append(failed, "secret_keys")
	}
	if s == nil || s.signKeys == nil || !s.signKeys.Ready(ctx, s.signingScopes) {
		failed = append(failed, "signing_keys")
	}
	if len(failed) > 0 {
		return health.Report{Status: health.StatusNotReady, Failed: failed}
	}
	return health.Report{Status: health.StatusReady}
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
	if s.xdsGRPC != nil && s.xdsLn != nil {
		go func() {
			errCh <- s.xdsGRPC.Serve(s.xdsLn)
		}()
	}
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
	go func() { s.serverCertificateRefreshLoop(runCtx); errCh <- nil }()
	if s.ingress != nil {
		go func() { errCh <- s.ingress.Follow(runCtx) }()
	}
	// Per-replica as well: every replica ingests the agent streams it
	// terminates.
	if s.logIngester != nil {
		go func() { errCh <- s.logIngester.Run(runCtx) }()
	}
	leaseDone := make(chan error, 1)
	go func() {
		advertise := strings.TrimSpace(s.cfg.AdvertiseAddr)
		if advertise == "" {
			advertise = s.InternalAddr()
		}
		s.leases.SetAdvertise(advertise)
		s.leases.SetFenceHooks(func() { s.store.publication.SetPublishing(false) }, func() { s.store.publication.SetPublishing(true) })
		err := s.leases.Run(runCtx, SingletonLeaseName, s.runSingletonJobs)
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
	if err := s.delivery.BecomeLive(ctx); err != nil {
		return err
	}
	defer s.delivery.ResignLive()
	if s.reconciler != nil {
		if err := s.reconciler.Bootstrap(ctx); err != nil {
			slog.Warn("github bootstrap reconcile failed", "error", err)
		}
	}
	errCh := make(chan error, 8)
	if s.webhooks != nil {
		go func() { errCh <- s.webhooks.Run(ctx) }()
	}
	go func() { errCh <- s.delivery.ServeLive(ctx) }()
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
	go func() { errCh <- s.journalCompactionLoop(ctx) }()
	go func() { s.sourceArchiveRetentionLoop(ctx); errCh <- nil }()
	go func() { errCh <- s.deletionGC(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) journalCompactionLoop(ctx context.Context) error {
	compact := func() {
		if _, err := s.store.compactJournal(ctx, journal.DefaultRetainEntries); err != nil && ctx.Err() == nil {
			slog.Warn("journal compaction failed", "error", err)
		}
	}
	compact()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			compact()
		}
	}
}

func (s *Server) buildLeaseRepairLoop(ctx context.Context) error {
	interval := s.buildStaleAfter / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	repair := func() {
		if err := s.delivery.RecoverExpiredBuilds(ctx); err != nil && ctx.Err() == nil {
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

func (s *Server) deletionGC(ctx context.Context) error {
	gc := NewDeletionGC(s.store, s.notifier, s.ingress, time.Duration(s.cfg.Deletion.GCIntervalSeconds)*time.Second)
	if s.logStore != nil {
		gc.SetLogPurgeHook(func(ctx context.Context, projectID string) error {
			return s.logStore.PurgeProjectLogs(ctx, projectID)
		})
	}
	return gc.Run(ctx)
}

func (s *Server) serverCertificateRefreshLoop(ctx context.Context) {
	refresh := func() {
		if s.authority == nil {
			return
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.authority.RefreshServerCertificate(refreshCtx); err != nil && ctx.Err() == nil {
			slog.Warn("server certificate refresh failed", "error", err)
		}
	}
	refresh()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (s *Server) sourceArchiveRetentionLoop(ctx context.Context) {
	prune := func() {
		now, err := dbtx.DatabaseTime(ctx, s.store.db)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("source archive retention database time failed", "error", err)
			}
			return
		}
		cutoff := now.AddDate(0, 0, -s.cfg.SourceArchives.RetentionDays)
		deleted, err := s.store.source.PruneSourceArchives(ctx, cutoff)
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
	if s.internalGRPC != nil {
		// All gRPC traffic is multiplexed through internalHTTP via ServeHTTP.
		// Stop the gRPC server first so active Sync streams are torn down;
		// otherwise HTTP Shutdown waits the full deadline for them to go idle.
		s.internalGRPC.Stop()
	}
	if s.internalHTTP != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.internalHTTP.Shutdown(shutdownCtx)
		cancel()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			if errors.Is(err, context.DeadlineExceeded) {
				closeErr := s.internalHTTP.Close()
				if errors.Is(closeErr, http.ErrServerClosed) {
					closeErr = nil
				}
				errs = append(errs, errors.Join(err, closeErr))
			} else {
				errs = append(errs, err)
			}
		}
	}
	if s.internalLn != nil {
		_ = s.internalLn.Close()
	}
	if s.xdsGRPC != nil {
		s.xdsGRPC.Stop()
	}
	if s.xdsLn != nil {
		_ = s.xdsLn.Close()
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
		if s.store.secrets != nil {
			errs = append(errs, s.store.secrets.Close())
		}
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

// XDSAddr is the address Envoy instances subscribe to.
func (s *Server) XDSAddr() string {
	if s == nil || s.xdsLn == nil {
		return ""
	}
	return s.xdsLn.Addr().String()
}

// XDSServer exposes the xDS management server for status inspection.
func (s *Server) XDSServer() *xds.Server {
	if s == nil {
		return nil
	}
	return s.xdsServer
}

func (s *Server) RegistryAuthBundlePath() string {
	if s == nil || s.registryAuth == nil {
		return ""
	}
	return s.registryAuth.BundlePath()
}

// SigningKeys exposes the shared signing-key inventory for operator tooling
// and provisioning flows (dashboard secret export).
func (s *Server) SigningKeys() *signkeys.Service {
	if s == nil {
		return nil
	}
	return s.signKeys
}

func (s *Server) EnsureDashboardClientIdentity(ctx context.Context, id string) (identity.ClientIdentityMaterial, error) {
	if s == nil || s.authority == nil {
		return identity.ClientIdentityMaterial{}, errors.New("controlplane authority is not initialized")
	}
	if allowedID := s.cfg.Dashboard.ServiceCallerID; allowedID == "" || id != allowedID {
		return identity.ClientIdentityMaterial{}, fmt.Errorf("dashboard client identity %q is not allowed", id)
	}
	return s.authority.EnsureDashboardClientIdentity(ctx, id)
}

func (s *Server) EnsureBuilderClientIdentity(ctx context.Context, id string) (identity.ClientIdentityMaterial, error) {
	if s == nil || s.authority == nil {
		return identity.ClientIdentityMaterial{}, errors.New("controlplane authority is not initialized")
	}
	return s.authority.EnsureBuilderClientIdentity(ctx, id)
}

// userAssertionSecrets verifies dashboard user assertions against the
// shared user-assertion key: active first, then the retiring key while a
// rotation overlaps.
func userAssertionSecrets(keys *signkeys.Service) identity.UserAssertionSecrets {
	return func(ctx context.Context) ([][]byte, error) {
		mats, err := keys.Verifying(ctx, signkeys.ScopeUserAssertion)
		if err != nil {
			return nil, err
		}
		var out [][]byte
		for _, mat := range mats {
			if mat.Record.KeyType != signkeys.KeyTypeHMAC256 {
				return nil, errors.New("user-assertion signing key is not an HMAC secret")
			}
			out = append(out, append([]byte(nil), mat.Private...))
		}
		return out, nil
	}
}

func (s *Server) HasHealthyAgent(ctx context.Context, agentID string) (bool, error) {
	if s == nil || s.store == nil {
		return false, errors.New("controlplane store is not initialized")
	}
	rec, err := s.store.reads.AgentByID(ctx, agentID)
	switch {
	case err == nil:
		return rec.Healthy(time.Now().UTC()), nil
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

func newConnectHandler(internalAuth *identity.InternalAuth, platformService platformv1connect.PlatformServiceHandler, opsService platformv1connect.OpsServiceHandler) http.Handler {
	options := []connect.HandlerOption{
		connect.WithInterceptors(internalAuth.ConnectInterceptor()),
	}
	mux := http.NewServeMux()
	mux.Handle(platformv1connect.NewPlatformServiceHandler(platformService, options...))
	mux.Handle(platformv1connect.NewOpsServiceHandler(opsService, options...))
	return mux
}

func dualProtocolHandler(grpcServer *grpc.Server, connectHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
			ctx = identity.WithVerifiedClientCertificate(ctx, r.TLS.VerifiedChains[0][0])
		}
		r = r.WithContext(ctx)
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		connectHandler.ServeHTTP(w, r)
	})
}
