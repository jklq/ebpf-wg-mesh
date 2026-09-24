package bootstrap

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"ebof-wg-mesh/internal/config"
)

func ControlPlane(args []string) (config.ControlPlaneConfig, error) {
	var cfg config.ControlPlaneConfig
	var profile string
	var bootstrapUsers []bootstrapUserSpec
	var dashboardUsers []bootstrapUserSpec
	var internalServerNames string
	var agentBootstrapTokens string
	var dashboardCommand string
	var dashboardArgs string
	var dashboardEnv string
	var dashboardContainerPort int
	var githubPrivateKeyFile string
	var replicaAddresses string
	var advertiseAddr string

	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	stringFlag(fs, &profile, "profile", "CONTROLPLANE_PROFILE", "", "development or production; empty defaults to production")
	stringFlag(fs, &cfg.Health.Listen, "health-listen", "CONTROLPLANE_HEALTH_LISTEN", "", "liveness and readiness listen address")
	stringFlag(fs, &cfg.InternalGRPC.Listen, "internal-listen", "CONTROLPLANE_INTERNAL_LISTEN", "0.0.0.0:9443", "")
	stringFlag(fs, &replicaAddresses, "replica-addresses", "CONTROLPLANE_REPLICA_ADDRESSES", "", "comma-separated control-plane addresses advertised to agents")
	stringFlag(fs, &advertiseAddr, "advertise-addr", "CONTROLPLANE_ADVERTISE_ADDR", "", "this replica's control-plane address as agents should dial it")
	stringFlag(fs, &internalServerNames, "internal-server-names", "CONTROLPLANE_INTERNAL_SERVER_NAMES", "controlplane,controlplane-internal,localhost", "")
	stringFlag(fs, &agentBootstrapTokens, "agent-bootstrap-tokens", "CONTROLPLANE_AGENT_BOOTSTRAP_TOKENS", "", "")
	intFlag(fs, &cfg.InternalGRPC.TLS.ServerCertValidityHours, "internal-server-cert-validity-hours", "CONTROLPLANE_INTERNAL_SERVER_CERT_VALIDITY_HOURS", 24*30, "")
	intFlag(fs, &cfg.InternalGRPC.TLS.ClientCertValidityHours, "internal-client-cert-validity-hours", "CONTROLPLANE_INTERNAL_CLIENT_CERT_VALIDITY_HOURS", 24, "")
	stringFlag(fs, &cfg.InternalGRPC.TLS.RevokedClientCertSerialsFile, "internal-revoked-client-cert-serials-file", "CONTROLPLANE_INTERNAL_REVOKED_CLIENT_CERT_SERIALS_FILE", "", "one hexadecimal client certificate serial per line; defaults under the control-plane state directory")
	stringFlag(fs, &cfg.Database.URL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &cfg.Logs.ClickHouse.URL, "logs-clickhouse-url", "CONTROLPLANE_LOGS_CLICKHOUSE_URL", "", "")
	intFlag(fs, &cfg.Logs.RetentionDays, "logs-retention-days", "CONTROLPLANE_LOGS_RETENTION_DAYS", 14, "")
	intFlag(fs, &cfg.Logs.IngestQueueBytes, "logs-ingest-queue-bytes", "CONTROLPLANE_LOGS_INGEST_QUEUE_BYTES", 64<<20, "retained bytes of the durable ingest journal before whole batches shed with gap rows")
	intFlag(fs, &cfg.Logs.IngestRatePerSec, "logs-ingest-rate-per-sec", "CONTROLPLANE_LOGS_INGEST_RATE_PER_SEC", 2000, "per-allocation ingest guard above agent-side limits")
	intFlag(fs, &cfg.Logs.IngestBurst, "logs-ingest-burst", "CONTROLPLANE_LOGS_INGEST_BURST", 10000, "")
	stringFlag(fs, &cfg.StateDir, "state-dir", "CONTROLPLANE_STATE_DIR", "var/controlplane", "")
	stringFlag(fs, &cfg.SourceArchives.Provider, "source-archives-provider", "CONTROLPLANE_SOURCE_ARCHIVES_PROVIDER", "", "file (development) or s3 (production); defaults to s3 when S3 fields are set, else file")
	stringFlag(fs, &cfg.SourceArchives.Directory, "source-archives-dir", "CONTROLPLANE_SOURCE_ARCHIVES_DIR", "", "defaults under the control-plane state directory; file provider only")
	intFlag(fs, &cfg.SourceArchives.RetentionDays, "source-archives-retention-days", "CONTROLPLANE_SOURCE_ARCHIVES_RETENTION_DAYS", 30, "")
	stringFlag(fs, &cfg.SourceArchives.S3.Endpoint, "source-archives-s3-endpoint", "CONTROLPLANE_SOURCE_ARCHIVES_S3_ENDPOINT", "", "S3-compatible endpoint URL, e.g. https://s3.us-east-1.amazonaws.com")
	stringFlag(fs, &cfg.SourceArchives.S3.Region, "source-archives-s3-region", "CONTROLPLANE_SOURCE_ARCHIVES_S3_REGION", "", "")
	stringFlag(fs, &cfg.SourceArchives.S3.Bucket, "source-archives-s3-bucket", "CONTROLPLANE_SOURCE_ARCHIVES_S3_BUCKET", "", "")
	stringFlag(fs, &cfg.SourceArchives.S3.Prefix, "source-archives-s3-prefix", "CONTROLPLANE_SOURCE_ARCHIVES_S3_PREFIX", "", "optional key prefix inside the bucket")
	stringFlag(fs, &cfg.SourceArchives.S3.ServerSideEncryption, "source-archives-s3-sse", "CONTROLPLANE_SOURCE_ARCHIVES_S3_SSE", "", "empty, AES256, or aws:kms")
	stringFlag(fs, &cfg.SourceArchives.S3.SSEKMSKeyID, "source-archives-s3-kms-key-id", "CONTROLPLANE_SOURCE_ARCHIVES_S3_KMS_KEY_ID", "", "")
	stringFlag(fs, &cfg.SourceArchives.S3.CredentialsFile, "source-archives-s3-credentials-file", "CONTROLPLANE_SOURCE_ARCHIVES_S3_CREDENTIALS_FILE", "", "JSON file with access_key_id/secret_access_key; empty uses workload identity or environment")
	intFlag(fs, &cfg.SourceArchives.S3.RequestTimeoutSeconds, "source-archives-s3-request-timeout-seconds", "CONTROLPLANE_SOURCE_ARCHIVES_S3_REQUEST_TIMEOUT_SECONDS", 30, "")
	intFlag(fs, &cfg.SourceArchives.S3.MaxRetries, "source-archives-s3-max-retries", "CONTROLPLANE_SOURCE_ARCHIVES_S3_MAX_RETRIES", 3, "")
	stringFlag(fs, &cfg.SecretKeys.KeyringPath, "secret-keys-keyring", "CONTROLPLANE_SECRET_KEYS_KEYRING", "", "provisioned master-key ring file; same contents on every replica, defaults under the state directory")
	intFlag(fs, &cfg.Deletion.GracePeriodDays, "deletion-grace-period-days", "CONTROLPLANE_DELETION_GRACE_PERIOD_DAYS", 7, "how long deleted resources stay restorable")
	intFlag(fs, &cfg.Deletion.GCIntervalSeconds, "deletion-gc-interval-seconds", "CONTROLPLANE_DELETION_GC_INTERVAL_SECONDS", 60, "how often expired tombstones are garbage-collected")
	stringFlag(fs, &cfg.Ingress.XDSListen, "ingress-xds-listen", "CONTROLPLANE_INGRESS_XDS_LISTEN", "127.0.0.1:18000", "xDS management API listen address for Envoy instances")
	stringFlag(fs, &cfg.Ingress.PublicAddr, "ingress-public-addr", "CONTROLPLANE_INGRESS_PUBLIC_ADDR", "platform.local", "")
	stringFlag(fs, &cfg.Ingress.ControlPlaneHTTPUpstream, "ingress-controlplane-upstream", "CONTROLPLANE_INGRESS_CONTROLPLANE_UPSTREAM", "127.0.0.1:8080", "")
	boolFlag(fs, &cfg.Dashboard.Enabled, "dashboard-enabled", "CONTROLPLANE_DASHBOARD_ENABLED", false, "")
	stringFlag(fs, &cfg.Dashboard.ProjectName, "dashboard-project-name", "CONTROLPLANE_DASHBOARD_PROJECT_NAME", "Platform Dashboard", "")
	stringFlag(fs, &cfg.Dashboard.ProjectSystemKey, "dashboard-project-system-key", "CONTROLPLANE_DASHBOARD_PROJECT_SYSTEM_KEY", "dashboard", "")
	stringFlag(fs, &cfg.Dashboard.ServiceName, "dashboard-service-name", "CONTROLPLANE_DASHBOARD_SERVICE_NAME", "dashboard", "")
	stringFlag(fs, &cfg.Dashboard.ServiceCallerID, "dashboard-service-caller-id", "CONTROLPLANE_DASHBOARD_SERVICE_CALLER_ID", "dashboard", "")
	stringFlag(fs, &cfg.Dashboard.TrustedAgentID, "dashboard-trusted-agent-id", "CONTROLPLANE_DASHBOARD_TRUSTED_AGENT_ID", "", "")
	stringFlag(fs, &cfg.Dashboard.PublicDomain, "dashboard-public-domain", "CONTROLPLANE_DASHBOARD_PUBLIC_DOMAIN", "", "")
	stringFlag(fs, &cfg.Dashboard.GitHubInstallURL, "dashboard-github-install-url", "CONTROLPLANE_DASHBOARD_GITHUB_INSTALL_URL", "", "")
	stringFlag(fs, &cfg.Dashboard.IngressTargetHost, "dashboard-ingress-target-host", "CONTROLPLANE_DASHBOARD_INGRESS_TARGET_HOST", "", "")
	stringFlag(fs, &cfg.Dashboard.ControlPlaneAddr, "dashboard-controlplane-addr", "CONTROLPLANE_DASHBOARD_CONTROLPLANE_ADDR", "controlplane:9443", "")
	stringFlag(fs, &cfg.Dashboard.ControlPlaneSNI, "dashboard-controlplane-sni", "CONTROLPLANE_DASHBOARD_CONTROLPLANE_SNI", "controlplane", "")
	stringFlag(fs, &cfg.Dashboard.Image, "dashboard-image", "CONTROLPLANE_DASHBOARD_IMAGE", "", "")
	stringFlag(fs, &dashboardCommand, "dashboard-command", "CONTROLPLANE_DASHBOARD_COMMAND", "", "")
	stringFlag(fs, &dashboardArgs, "dashboard-args", "CONTROLPLANE_DASHBOARD_ARGS", "", "")
	stringFlag(fs, &dashboardEnv, "dashboard-env", "CONTROLPLANE_DASHBOARD_ENV", "", "")
	intFlag(fs, &dashboardContainerPort, "dashboard-container-port", "CONTROLPLANE_DASHBOARD_CONTAINER_PORT", 3000, "")
	stringFlag(fs, &cfg.Dashboard.HealthPath, "dashboard-health-path", "CONTROLPLANE_DASHBOARD_HEALTH_PATH", "/healthz", "")
	int64Flag(fs, &cfg.Dashboard.CPUMillis, "dashboard-cpu-millis", "CONTROLPLANE_DASHBOARD_CPU_MILLIS", 250, "")
	int64Flag(fs, &cfg.Dashboard.MemoryMebibytes, "dashboard-memory-mebibytes", "CONTROLPLANE_DASHBOARD_MEMORY_MEBIBYTES", 256, "")
	stringFlag(fs, &cfg.Dashboard.DatabaseSchema, "dashboard-db-schema", "CONTROLPLANE_DASHBOARD_DB_SCHEMA", "dashboard", "")
	stringFlag(fs, &cfg.Dashboard.SessionCookieName, "dashboard-session-cookie-name", "CONTROLPLANE_DASHBOARD_SESSION_COOKIE_NAME", "dashboard_session", "")
	boolFlag(fs, &cfg.GitHub.Enabled, "github-enabled", "CONTROLPLANE_GITHUB_ENABLED", false, "")
	int64Flag(fs, &cfg.GitHub.AppID, "github-app-id", "CONTROLPLANE_GITHUB_APP_ID", 0, "")
	stringFlag(fs, &cfg.GitHub.WebhookSecret, "github-webhook-secret", "CONTROLPLANE_GITHUB_WEBHOOK_SECRET", "", "")
	stringFlag(fs, &githubPrivateKeyFile, "github-private-key-file", "CONTROLPLANE_GITHUB_PRIVATE_KEY_FILE", "", "")
	stringFlag(fs, &cfg.GitHub.APIBaseURL, "github-api-base-url", "CONTROLPLANE_GITHUB_API_BASE_URL", "https://api.github.com", "")
	stringFlag(fs, &cfg.GitHub.WebhookPath, "github-webhook-path", "CONTROLPLANE_GITHUB_WEBHOOK_PATH", "/webhooks/github", "")
	stringFlag(fs, &cfg.Registry.Host, "registry-host", "CONTROLPLANE_REGISTRY_HOST", "", "")
	stringFlag(fs, &cfg.Registry.NamespacePrefix, "registry-namespace-prefix", "CONTROLPLANE_REGISTRY_NAMESPACE_PREFIX", "mesh", "")
	stringFlag(fs, &cfg.Registry.AuthListen, "registry-auth-listen", "CONTROLPLANE_REGISTRY_AUTH_LISTEN", "127.0.0.1:9444", "embedded registry token service listen address")
	stringFlag(fs, &cfg.Registry.TokenIssuer, "registry-token-issuer", "CONTROLPLANE_REGISTRY_TOKEN_ISSUER", "ebpf-wg-mesh", "issuer configured on the registry token verifier")
	stringFlag(fs, &cfg.Registry.TokenService, "registry-token-service", "CONTROLPLANE_REGISTRY_TOKEN_SERVICE", "", "registry token audience; defaults to registry-host")
	intFlag(fs, &cfg.Registry.CredentialTTLSeconds, "registry-credential-ttl-seconds", "CONTROLPLANE_REGISTRY_CREDENTIAL_TTL_SECONDS", 300, "")
	intFlag(fs, &cfg.Registry.PullCredentialTTLSeconds, "registry-pull-credential-ttl-seconds", "CONTROLPLANE_REGISTRY_PULL_CREDENTIAL_TTL_SECONDS", 48*3600, "pull capability lifetime; must exceed the client certificate lifetime")
	builderDefaults := config.DefaultControlPlaneBuilderConfig()
	intFlag(fs, &cfg.Builder.HeartbeatTimeoutSeconds, "builder-heartbeat-timeout-seconds", "CONTROLPLANE_BUILDER_HEARTBEAT_TIMEOUT_SECONDS", builderDefaults.HeartbeatTimeoutSeconds, "")
	intFlag(fs, &cfg.Builder.LeaseTTLSeconds, "builder-lease-ttl-seconds", "CONTROLPLANE_BUILDER_LEASE_TTL_SECONDS", builderDefaults.LeaseTTLSeconds, "build ownership lease duration")
	intFlag(fs, &cfg.Builder.MaxAttempts, "builder-max-attempts", "CONTROLPLANE_BUILDER_MAX_ATTEMPTS", builderDefaults.MaxAttempts, "")
	intFlag(fs, &cfg.Builder.MaxConcurrentGlobal, "builder-max-concurrent-global", "CONTROLPLANE_BUILDER_MAX_CONCURRENT_GLOBAL", builderDefaults.MaxConcurrentGlobal, "")
	intFlag(fs, &cfg.Builder.MaxConcurrentPerProject, "builder-max-concurrent-per-project", "CONTROLPLANE_BUILDER_MAX_CONCURRENT_PER_PROJECT", builderDefaults.MaxConcurrentPerProject, "")
	intFlag(fs, &cfg.Builder.BuildTimeoutSeconds, "builder-timeout-seconds", "CONTROLPLANE_BUILDER_TIMEOUT_SECONDS", builderDefaults.BuildTimeoutSeconds, "")
	intFlag(fs, &cfg.Builder.MaxQueueAgeSeconds, "builder-max-queue-age-seconds", "CONTROLPLANE_BUILDER_MAX_QUEUE_AGE_SECONDS", builderDefaults.MaxQueueAgeSeconds, "")
	intFlag(fs, &cfg.Failover.ReconcileIntervalSeconds, "failover-reconcile-interval-seconds", "CONTROLPLANE_FAILOVER_RECONCILE_INTERVAL_SECONDS", 5, "")
	intFlag(fs, &cfg.Failover.UnhealthyThresholdSeconds, "failover-unhealthy-threshold-seconds", "CONTROLPLANE_FAILOVER_UNHEALTHY_THRESHOLD_SECONDS", 30, "")
	stringFlag(fs, &cfg.Mesh.InterfaceName, "mesh-interface-name", "CONTROLPLANE_MESH_INTERFACE_NAME", "wg0", "")
	intFlag(fs, &cfg.Mesh.ListenPort, "mesh-listen-port", "CONTROLPLANE_MESH_LISTEN_PORT", 51820, "")
	stringFlag(fs, &cfg.Mesh.NetworkCIDR, "mesh-network-cidr", "CONTROLPLANE_MESH_NETWORK_CIDR", "fd00:44::/64", "")
	stringFlag(fs, &cfg.Mesh.WorkloadPoolCIDR, "mesh-workload-pool-cidr", "CONTROLPLANE_MESH_WORKLOAD_POOL_CIDR", "fd00:200::/48", "")
	stringFlag(fs, &cfg.Mesh.WorkloadIPv4PoolCIDR, "mesh-workload-ipv4-pool-cidr", "CONTROLPLANE_MESH_WORKLOAD_IPV4_POOL_CIDR", "10.200.0.0/16", "")
	intFlag(fs, &cfg.Mesh.WorkloadIPv4NodePrefixBits, "mesh-workload-ipv4-node-prefix-bits", "CONTROLPLANE_MESH_WORKLOAD_IPV4_NODE_PREFIX_BITS", 24, "")
	intFlag(fs, &cfg.Mesh.PersistentKeepaliveSeconds, "mesh-persistent-keepalive-seconds", "CONTROLPLANE_MESH_PERSISTENT_KEEPALIVE_SECONDS", 5, "")
	fs.Var(bootstrapUsersFlag{users: &bootstrapUsers}, "bootstrap-user", "user-id:email[:project1,project2][+operator]")
	fs.Var(bootstrapUsersFlag{users: &dashboardUsers}, "dashboard-dev-user", "user-id:email")

	if err := fs.Parse(args); err != nil {
		return config.ControlPlaneConfig{}, err
	}
	normalized, err := config.NormalizeProfile(profile)
	if err != nil {
		return config.ControlPlaneConfig{}, err
	}
	cfg.Profile = normalized
	cfg.InternalGRPC.TLS.ServerNames = splitCommaList(internalServerNames)
	cfg.ReplicaAddresses = splitCommaList(replicaAddresses)
	cfg.AdvertiseAddr = strings.TrimSpace(advertiseAddr)
	cfg.InternalGRPC.TLS.BootstrapTokens, err = parseAgentBootstrapTokens(agentBootstrapTokens)
	if err != nil {
		return config.ControlPlaneConfig{}, err
	}
	cfg.Dashboard.ContainerPort = int32(dashboardContainerPort)
	cfg.Dashboard.Command = splitWhitespaceList(dashboardCommand)
	cfg.Dashboard.Args = splitWhitespaceList(dashboardArgs)
	cfg.Dashboard.Env = parseEnvPairs(dashboardEnv)
	if githubPrivateKeyFile != "" {
		privateKeyPEM, err := os.ReadFile(githubPrivateKeyFile)
		if err != nil {
			return config.ControlPlaneConfig{}, fmt.Errorf("read GitHub private key %s: %w", githubPrivateKeyFile, err)
		}
		cfg.GitHub.PrivateKeyPEM = string(privateKeyPEM)
	}
	for _, user := range bootstrapUsers {
		cfg.Bootstrap.Users = append(cfg.Bootstrap.Users, config.BootstrapUser{
			ID:       user.userID,
			Email:    user.email,
			Projects: append([]string(nil), user.projects...),
			Operator: user.operator,
		})
	}
	for _, user := range dashboardUsers {
		cfg.Dashboard.DevUsers = append(cfg.Dashboard.DevUsers, config.BootstrapUser{
			ID:    user.userID,
			Email: user.email,
		})
	}
	if err := config.FinalizeControlPlane(&cfg); err != nil {
		return config.ControlPlaneConfig{}, fmt.Errorf("bootstrap control plane: %w", err)
	}
	return cfg, nil
}
