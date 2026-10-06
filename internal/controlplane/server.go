// Package controlplane assembles the platform runtime and adapts authenticated
// transports to delivery, catalog, routing, and identity operations.
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ebof-wg-mesh/internal/controlplane/certificates"
	"ebof-wg-mesh/internal/controlplane/dbtx"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/ingressnodes"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/logs"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/routing"
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
	notifier        *notifier
	authority       *identity.TLSAuthority
	internalGRPC    *grpc.Server
	internalHTTP    *http.Server
	ingress         *xds.Publisher
	certificates    *certificates.Service
	xdsServer       *xds.Server
	xdsGRPC         *grpc.Server
	xdsLn           net.Listener
	dashboard       *managedDashboardReconciler
	registry        *registry.Policy
	registryAuth    *registry.Auth
	registryHTTP    *http.Server
	github          *source.GitHubCatalog
	webhooks        *source.GitHubWebhookProcessor
	coordinator     *source.GitHubCoordinator
	reconciler      *source.GitHubReconciler
	rollouts        *rolloutReconciler
	failover        *serviceFailoverReconciler
	leases          *leaseManager
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
		withDeletionGracePeriod(time.Duration(cfg.Deletion.GracePeriodDays)*24*time.Hour))
	if err != nil {
		return nil, err
	}
	secrets, err := secretkeys.Open(ctx, store.db, cfg.SecretKeys, secretkeys.Options{
		// Production never generates keys: the keyring is explicitly provisioned, Open fails closed.
		AllowGenerate: !cfg.Profile.IsProduction(),
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open envelope keys: %w", err)
	}
	store.attachSecrets(secrets)
	signKeys, err := signkeys.Open(ctx, store.db, secrets, signkeys.Options{
		// Production never generates keys; scopes initialize via the signing-keys CLI.
		AllowGenerate:   !cfg.Profile.IsProduction(),
		RegistryEnabled: strings.TrimSpace(cfg.Registry.Host) != "",
	})
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open signing keys: %w", err)
	}
	leases := newLeaseManager(store.database, 15*time.Second, time.Second)
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
	if strings.EqualFold(strings.TrimSpace(cfg.SourceArchives.Provider), config.SourceArchiveProviderFile) {
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
	if err := store.source.ConfigureSourceArchives(archiveStore, filepath.Join(cfg.StateDir, "source-archive-stage")); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure source archive staging: %w", err)
	}
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
	if cfg.Logs.File.Directory == "" {
		cfg.Logs.File.Directory = filepath.Join(cfg.StateDir, "logs")
	}
	logStore, err := logs.OpenLogStore(ctx, cfg.Logs)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	var logIngester *logs.AsyncIngester
	logsOwnedByServer := false
	defer func() {
		if !logsOwnedByServer {
			_ = logIngester.Close()
			_ = logStore.Close()
			_ = store.Close()
		}
	}()
	if logStore != nil {
		logStore.SetProjectResolver(store.catalog.resolveLogRetention)
	}
	// Each replica journals into its own directory: replicas share the state volume but never
	// the spool files, so failover can't race two drainers over one journal.
	replicaID := strings.NewReplacer(":", "_", "/", "_").Replace(cfg.AdvertiseAddr)
	if replicaID == "" {
		replicaID = "default"
	}
	logIngester, err = logs.NewAsyncIngester(logStore, logs.AsyncIngesterConfig{
		SpoolDir:   filepath.Join(cfg.Logs.File.Directory, "ingest", replicaID),
		QueueBytes: int64(cfg.Logs.IngestQueueBytes),
		RatePerSec: float64(cfg.Logs.IngestRatePerSec),
		Burst:      cfg.Logs.IngestBurst,
	})
	if err != nil {
		return nil, err
	}
	logEmitter := logs.NewLogEmitter(logStore, logIngester)
	notifier := newNotifier(store.notifications)
	platformEvents := newPlatformEvents(store.database, 0)
	staticRoutes := make([]xds.StaticRoute, 0, len(cfg.Ingress.StaticRoutes))
	for _, route := range cfg.Ingress.StaticRoutes {
		staticRoutes = append(staticRoutes, xds.StaticRoute{
			Hosts:    append([]string(nil), route.Hosts...),
			Upstream: route.Upstream,
		})
	}
	for _, route := range cfg.Ingress.StaticRoutes {
		store.routing.staticHosts = append(store.routing.staticHosts, route.Hosts...)
	}
	certs, err := newCertificateService(cfg, store)
	if err != nil {
		return nil, err
	}
	xdsServer := xds.NewServer(context.Background())
	nodeRegistry := ingressnodes.New(store.db)
	xdsServer.SetNodeStore(nodeRegistry)
	publisherID := strings.TrimSpace(cfg.AdvertiseAddr)
	if publisherID == "" {
		publisherID = strings.TrimSpace(cfg.InternalGRPC.Listen)
	}
	ingress := xds.NewPublisher(xds.PublisherConfig{
		Source:           ingressInputs{routing: store.routing, certificates: certs},
		Publications:     store.routing,
		Nodes:            nodeRegistry,
		Server:           xdsServer,
		Keys:             certs,
		Static:           staticRoutes,
		HTTPListenAddrs:  cfg.Ingress.HTTPListenAddrs,
		HTTPSListenAddrs: cfg.Ingress.HTTPSListenAddrs,
		PublisherID:      publisherID,
	})
	certs.SetIngress(ingress)
	xdsServer.SetFirstContactHook(ingress.Refresh)
	policy := registry.NewPolicy(cfg.Registry, registryAuth)
	scheduler := buildSchedulerConfigFromControlPlane(cfg.Builder)
	delivery := newDeliveryWithScheduler(store, &scheduler, notifier, ingress, platformEvents, logEmitter)
	delivery.SetImageResolver(registry.NewHTTPResolver(nil, cfg.DirectImages.AllowedPrivateRegistryHosts))
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

	platformService := newPlatformService(
		store.platform(),
		notifier,
		ingress,
		delivery,
		withServiceLogs(logStore),
		withServiceLogEmitter(logEmitter),
		withGitHubSourceInspection(githubCatalog, githubClient, store.authorizer()),
		withPlatformDomainSuffix(cfg.Ingress.PublicAddr),
		withCertificates(certs),
		withPlatformEvents(platformEvents),
		withPlatformLiveOwner(leaseLiveOwner{leases: leases, name: singletonLeaseName}),
	)
	internalAuth := identity.NewInternalAuth(cfg.Dashboard.ServiceCallerID, userAssertionSecrets(signKeys), authority.Revocations())
	dashboard := newManagedDashboardReconciler(cfg.Dashboard, cfg.Profile, store.catalog, delivery, ingress, notifier)
	rollouts := newRolloutReconciler(delivery, 2*time.Second)
	failover := newServiceFailoverReconciler(delivery,
		time.Duration(cfg.Failover.ReconcileIntervalSeconds)*time.Second,
		time.Duration(cfg.Failover.UnhealthyThresholdSeconds)*time.Second)
	internal := grpc.NewServer(
		grpc.UnaryInterceptor(internalAuth.UnaryServerInterceptor()),
		grpc.StreamInterceptor(internalAuth.StreamServerInterceptor()),
	)
	agentv1.RegisterAgentControlServer(internal, newAgentService(
		store.fleet, delivery, logStore, notifier, authority, dashboard,
		cfg.Dashboard.Enabled, cfg.Dashboard.TrustedAgentID, cfg.Dashboard.ServiceCallerID,
		withAgentRegistry(policy),
		withReplicaAddresses(cfg.ReplicaAddresses),
		withLiveOwner(leaseLiveOwner{leases: leases, name: singletonLeaseName}),
		withLogIngester(logIngester),
	))
	platformv1.RegisterPlatformServiceServer(internal, platformService)
	buildOperations := newBuildOperations(store.db, store.source, delivery, policy, policy, withBuilderLogEmitter(logEmitter))
	platformv1.RegisterBuilderServiceServer(internal, newBuilderService(buildOperations))
	opsService := newOpsService(webhookHandler, store.fleet, delivery, notifier, authority)
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
	xdsGRPC := xdsServer.GRPCServer(authority.XDSConfig(), authority.Revocations())
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
		certificates:    certs,
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
	logsOwnedByServer = true
	return server, nil
}

// newCertificateService issues certificates over ACME when a directory is set
// and serves the operator wildcard certificate when its files are set.
func newCertificateService(cfg config.ControlPlaneConfig, store *persistence) (*certificates.Service, error) {
	var issuer certificates.Issuer
	if strings.TrimSpace(cfg.Ingress.TLS.ACME.DirectoryURL) != "" {
		acme, err := certificates.NewACMEIssuer(cfg.Ingress.TLS.ACME, store.certificates)
		if err != nil {
			return nil, fmt.Errorf("configure ACME: %w", err)
		}
		issuer = acme
	}
	resolver := routing.NewPublicDNSResolver()
	certs, err := certificates.New(certificates.Config{
		Store:  store.certificates,
		Hosts:  store.routing,
		Issuer: issuer,
		Verify: func(ctx context.Context, hostname, platformHostname string) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return routing.VerifyOwnership(ctx, resolver, hostname, platformHostname)
		},
		PlatformSuffix:             cfg.Ingress.PublicAddr,
		PlatformCertFile:           cfg.Ingress.TLS.PlatformCertFile,
		PlatformKeyFile:            cfg.Ingress.TLS.PlatformKeyFile,
		RequirePlatformCertificate: cfg.Profile.IsProduction(),
	})
	if err != nil {
		return nil, fmt.Errorf("configure ingress certificates: %w", err)
	}
	return certs, nil
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
	// Per-replica as well: every replica ingests the agent streams it terminates. Run drains
	// accepted batches before returning; Close tears down the log store once runDone closes.
	var logIngestDone chan error
	if s.logIngester != nil {
		done := make(chan error, 1)
		logIngestDone = done
		go func() {
			err := s.logIngester.Run(runCtx)
			errCh <- err
			done <- err
		}()
	}
	leaseDone := make(chan error, 1)
	go func() {
		advertise := strings.TrimSpace(s.cfg.AdvertiseAddr)
		if advertise == "" {
			advertise = s.InternalAddr()
		}
		s.leases.SetAdvertise(advertise)
		s.leases.SetFenceHooks(func() { s.store.publication.SetPublishing(false) }, func() { s.store.publication.SetPublishing(true) })
		err := s.leases.Run(runCtx, singletonLeaseName, s.runSingletonJobs)
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
	// The shutdown drain flushes accepted batches into the log store; Close closes that store
	// right after runDone, so Run must not return until the drain finished.
	if logIngestDone != nil {
		if err := <-logIngestDone; result == nil {
			result = err
		}
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
	if s.certificates != nil {
		go func() { errCh <- s.certificates.Run(ctx) }()
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
	go func() { s.buildArtifactRetentionLoop(ctx); errCh <- nil }()
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
	gc := newDeletionGC(s.store, s.notifier, s.ingress, time.Duration(s.cfg.Deletion.GCIntervalSeconds)*time.Second)
	if s.logStore != nil {
		gc.SetLogPurgeHook(s.logStore.PurgeProjectLogs)
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

// buildArtifactRetentionLoop prunes unreferenced build artifacts past the retention window.
// Referenced artifacts are rollback material and never pruned; only unreferenced ones age out.
func (s *Server) buildArtifactRetentionLoop(ctx context.Context) {
	prune := func() {
		now, err := dbtx.DatabaseTime(ctx, s.store.db)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("build artifact retention database time failed", "error", err)
			}
			return
		}
		cutoff := now.AddDate(0, 0, -s.cfg.BuildArtifacts.RetentionDays)
		deleted, err := s.delivery.PruneBuildArtifacts(ctx, cutoff, s.cfg.BuildArtifacts.KeepRecent)
		if err != nil && ctx.Err() == nil {
			slog.Warn("build artifact retention failed", "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("build artifact retention completed", "artifacts_deleted", deleted)
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
	if s.internalGRPC != nil {
		// All gRPC traffic is multiplexed through internalHTTP via ServeHTTP. Stop the gRPC server
		// first: active Sync streams tear down so the ingest drain races no new admissions, and HTTP
		// Shutdown doesn't wait the full deadline for idle streams.
		s.internalGRPC.Stop()
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
	if s.logIngester != nil {
		errs = append(errs, s.logIngester.Close())
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

func (s *Server) RegistryAuthBundlePath() string {
	if s == nil || s.registryAuth == nil {
		return ""
	}
	return s.registryAuth.BundlePath()
}

// SigningKeys exposes the shared signing-key inventory for operator tooling.
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

// userAssertionSecrets verifies dashboard user assertions against the shared user-assertion
// key: active first, then the retiring key while a rotation overlaps.
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

// ProvisionIngressIdentity establishes membership before exposing credentials.
// It is an operator provisioning entry, used by the local deployment harness.
func (s *Server) ProvisionIngressIdentity(ctx context.Context, id string) (identity.ClientIdentityMaterial, error) {
	material, err := identity.IssueClientCertificate(ctx, s.signKeys, identity.CallerIngress, id, 24*time.Hour)
	if err != nil {
		return identity.ClientIdentityMaterial{}, err
	}
	if err := ingressnodes.New(s.store.db).Register(ctx, id); err != nil {
		return identity.ClientIdentityMaterial{}, err
	}
	return material, nil
}
