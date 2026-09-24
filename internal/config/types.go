package config

import "ebof-wg-mesh/internal/meshlabels"

type Profile string

const (
	ProfileDevelopment Profile = "development"
	ProfileProduction  Profile = "production"
)

func (p Profile) IsProduction() bool {
	return p == ProfileProduction || p == ""
}

type HealthConfig struct {
	Listen string
}

type ServerTLSConfig struct {
	ServerNames                  []string
	BootstrapTokens              []AgentBootstrapToken
	ServerCertValidityHours      int
	ClientCertValidityHours      int
	RevokedClientCertSerialsFile string
}

type AgentBootstrapToken struct {
	AgentID                 string
	Token                   string
	Name                    string
	Region                  string
	Zone                    string
	FailureDomain           string
	ReservedCPUMillis       int64
	ReservedMemoryMebibytes int64
}

type ClientTLSConfig struct {
	CAFile             string
	ServerName         string
	BootstrapToken     string
	RenewBeforeMinutes int
}

type ListenerConfig struct {
	Listen string
	TLS    ServerTLSConfig
}

type DatabaseConfig struct {
	URL          string
	MaxOpenConns int
	MaxIdleConns int
}

type ClickHouseConfig struct {
	URL          string
	MaxOpenConns int
	MaxIdleConns int
}

type LogCaptureConfig struct {
	ClickHouse    ClickHouseConfig
	RetentionDays int
	// IngestQueueFlushes caps queued agent batches in the
	// control-plane ingest queue. Past the cap whole batches shed
	// with explicit gap rows.
	IngestQueueFlushes int
	// IngestQueueBytes caps the retained size of queued batches so a
	// backend outage can never make the queue hold a memory-limited
	// control plane hostage. Past the budget whole batches shed with
	// explicit gap rows.
	IngestQueueBytes int
	// IngestRatePerSec and IngestBurst bound the per-allocation
	// ingest guard, which sits above agent-side limits.
	IngestRatePerSec int
	IngestBurst      int
}

type BootstrapConfig struct {
	Users []BootstrapUser
}

type BootstrapUser struct {
	ID       string
	Email    string
	Projects []string
	Operator bool
}

type IngressConfig struct {
	// XDSListen is the address the control plane serves the xDS
	// management API on for Envoy instances.
	XDSListen                string
	ListenAddrs              []string
	StaticRoutes             []StaticIngressRouteConfig
	PublicAddr               string
	ControlPlaneHTTPUpstream string
}

type StaticIngressRouteConfig struct {
	Hosts    []string
	Upstream string
}

type ManagedDashboardConfig struct {
	Enabled           bool
	ProjectName       string
	ProjectSystemKey  string
	ServiceName       string
	ServiceCallerID   string
	TrustedAgentID    string
	PublicDomain      string
	GitHubInstallURL  string
	IngressTargetHost string
	ControlPlaneAddr  string
	ControlPlaneSNI   string
	Image             string
	Command           []string
	Args              []string
	Env               map[string]string
	ContainerPort     int32
	HealthPath        string
	CPUMillis         int64
	MemoryMebibytes   int64
	DatabaseSchema    string
	SessionCookieName string
	DevUsers          []BootstrapUser
}

type GitHubAppConfig struct {
	Enabled       bool
	AppID         int64
	WebhookSecret string
	PrivateKeyPEM string
	APIBaseURL    string
	WebhookPath   string
}

type RegistryConfig struct {
	Host                     string
	NamespacePrefix          string
	AuthListen               string
	TokenIssuer              string
	TokenService             string
	CredentialTTLSeconds     int
	PullCredentialTTLSeconds int
}

type ControlPlaneBuilderConfig struct {
	HeartbeatTimeoutSeconds int
	MaxAttempts             int
	MaxConcurrentGlobal     int
	MaxConcurrentPerProject int
	BuildTimeoutSeconds     int
	MaxQueueAgeSeconds      int
}

type ControlPlaneFailoverConfig struct {
	ReconcileIntervalSeconds  int
	UnhealthyThresholdSeconds int
}

const (
	SourceArchiveProviderFile = "file"
	SourceArchiveProviderS3   = "s3"
)

type SourceArchiveS3Config struct {
	Endpoint              string
	Region                string
	Bucket                string
	Prefix                string
	ServerSideEncryption  string
	SSEKMSKeyID           string
	CredentialsFile       string
	RequestTimeoutSeconds int
	MaxRetries            int
}

type SourceArchiveConfig struct {
	Provider      string
	Directory     string
	RetentionDays int
	S3            SourceArchiveS3Config
}

// SecretKeysConfig points at the provisioned master-key ring for sealed
// service secrets. The same file contents must reach every control-plane
// replica; the database holds ciphertext and key-version metadata only.
type SecretKeysConfig struct {
	// KeyringPath is the access-restricted keyring file. Development
	// defaults it under the state directory and bootstraps first-install
	// keys; production requires the operator to provision it.
	KeyringPath string
}

// DeletionConfig tunes safe deletion: user deletes tombstone resources for
// the grace period (restorable), then background garbage collection destroys
// expired tombstones on its interval.
type DeletionConfig struct {
	GracePeriodDays   int
	GCIntervalSeconds int
}

type ControlPlaneConfig struct {
	Profile          Profile
	Health           HealthConfig
	InternalGRPC     ListenerConfig
	ReplicaAddresses []string
	AdvertiseAddr    string
	Database         DatabaseConfig
	Logs             LogCaptureConfig
	StateDir         string
	SourceArchives   SourceArchiveConfig
	SecretKeys       SecretKeysConfig
	Deletion         DeletionConfig
	Ingress          IngressConfig
	Dashboard        ManagedDashboardConfig
	Bootstrap        BootstrapConfig
	GitHub           GitHubAppConfig
	Registry         RegistryConfig
	Builder          ControlPlaneBuilderConfig
	Failover         ControlPlaneFailoverConfig
	Mesh             ControlPlaneMeshConfig
}

type ControlPlaneMeshConfig struct {
	InterfaceName              string
	ListenPort                 int
	NetworkCIDR                string
	WorkloadPoolCIDR           string
	WorkloadIPv4PoolCIDR       string
	WorkloadIPv4NodePrefixBits int
	PersistentKeepaliveSeconds int
}

type NodeConfig struct {
	ID            string
	Name          string
	AdvertiseAddr string
	Resources     NodeResourcesConfig
}

type NodeResourcesConfig struct {
	CPUMillis               int64
	MemoryMebibytes         int64
	ReservedCPUMillis       int64
	ReservedMemoryMebibytes int64
}

func (r NodeResourcesConfig) AdvertisedCPUMillis() int64 {
	return r.CPUMillis - r.ReservedCPUMillis
}

func (r NodeResourcesConfig) AdvertisedMemoryMebibytes() int64 {
	return r.MemoryMebibytes - r.ReservedMemoryMebibytes
}

type ControlPlaneClientConfig struct {
	Addresses []string
	TLS       ClientTLSConfig
}

type InternalClientTLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

type RuntimeConfig struct {
	DataDir                    string
	VolumesDir                 string
	ManagedDashboardSecretsDir string
	Snapshotter                string
	DisableCgroups             bool
}

type MeshConfig struct {
	Host      HostConfig
	WireGuard WireGuard
	Firewall  FirewallConfig
}

type AgentConfig struct {
	Profile      Profile
	Health       HealthConfig
	Node         NodeConfig
	ControlPlane ControlPlaneClientConfig
	Runtime      RuntimeConfig
	Containerd   ContainerdConfig
	Mesh         MeshConfig
	Logs         AgentLogShippingConfig
}

// AgentLogShippingConfig bounds the agent's durable log pipeline.
// Zero values select documented defaults, except RatePerSec: zero
// disables producer limiting (validation rejects negatives).
type AgentLogShippingConfig struct {
	// SpoolMaxBytes caps the disk spool under the runtime data dir.
	// Past the cap the oldest unshipped lines shed with explicit gap
	// rows. Defaults to 256 MiB.
	SpoolMaxBytes int64
	// RatePerSec and Burst bound accepted lines per allocation per
	// second. Past the limit lines shed with explicit gap rows.
	// Defaults to 200/s with bursts of 1000; zero RatePerSec
	// disables producer limiting entirely.
	RatePerSec int
	Burst      int
	// FlushBatchSize and FlushIntervalSeconds pace Sync stream
	// batches. Defaults to 100 lines and 1 second.
	FlushBatchSize       int
	FlushIntervalSeconds int
	// ReplayWindowSeconds bounds the spool replay after a
	// reconnect. Defaults to 300.
	ReplayWindowSeconds int
}

type BuilderControlPlaneConfig struct {
	Address string
	TLS     InternalClientTLSConfig
}

type BuilderConfig struct {
	Profile                  Profile
	Health                   HealthConfig
	ID                       string
	Name                     string
	ControlPlane             BuilderControlPlaneConfig
	WorkDir                  string
	PollIntervalSeconds      int
	HeartbeatIntervalSeconds int
	Logs                     BuilderLogShippingConfig
	Executor                 string
	BuildctlBinary           string
	BuildkitAddress          string
	RailpackBinary           string
	RailpackFrontendImage    string
	Limits                   BuilderLimitsConfig
	Network                  BuilderNetworkConfig
	Cache                    BuilderCacheConfig
	Sandbox                  BuilderSandboxConfig
	CleanupWorkDir           bool
}

// BuilderLogShippingConfig bounds the builder's durable build-log
// pipeline. Zero values select documented defaults, except
// RatePerSec: zero disables producer limiting (validation rejects
// negatives).
type BuilderLogShippingConfig struct {
	// SpoolMaxBytes caps the per-attempt disk spool under the
	// builder work dir. Defaults to 64 MiB.
	SpoolMaxBytes int64
	// RatePerSec and Burst bound accepted lines per build per
	// second. Defaults to 200/s with bursts of 1000; zero RatePerSec
	// disables producer limiting entirely.
	RatePerSec int
	Burst      int
	// FlushBatchSize and FlushIntervalSeconds pace ReportBuildLogs
	// calls. Defaults to 100 lines and 1 second.
	FlushBatchSize       int
	FlushIntervalSeconds int
}

// BuilderLimitsConfig carries the explicit per-execution resource
// limits every build receives as executor input.
type BuilderLimitsConfig struct {
	TimeoutSeconds    int
	MemoryBytes       int64
	CPUSeconds        int64
	MaxFileBytes      int64
	MaxProcesses      int64
	MaxWorkspaceBytes int64
}

// BuilderNetworkConfig carries the restricted network policy every
// build receives as executor input.
type BuilderNetworkConfig struct {
	// DenyGeneralEgress is inverted so the zero value preserves the
	// current behavior of allowing dependency fetches during builds.
	DenyGeneralEgress bool
	DeniedCIDRs       []string
}

// BuilderCacheConfig selects how build cache data persists between
// executions. Mode "none" persists nothing; mode "content-addressed"
// lets the hardened executor mount a host cache dir keyed purely by
// build content (snapshot digest, recipe, toolchain), never by
// project identity. Cache dirs accumulate under the builder work dir;
// the operator prunes them. The development executor validates the
// mode but exports no cache.
type BuilderCacheConfig struct {
	Mode string
}

// BuilderSandboxConfig selects the hardened executor's sandbox
// backend. The development executor ignores it. Only the containerd
// backend exists today; the operator selects the sandbox technology
// through Runtime (the default runc runtime, gVisor's runsc, Kata, or
// another installed OCI runtime) and the build CNI network. Unknown
// backends fail validation: execution never silently falls back to a
// weaker backend.
type BuilderSandboxConfig struct {
	// Backend is the sandbox backend. Only "containerd" exists.
	Backend string
	// Socket is the containerd socket the builder dials.
	Socket string
	// Namespace is the containerd namespace holding build sandboxes.
	// It must not be the workload namespace: build sandboxes are
	// one-shot and carry no mesh identity.
	Namespace string
	// Image is the sandbox image. It must provide the build
	// toolchain (buildctl, railpack when railpack builds run); the
	// executor bind-mounts the execution workspace at /build. There
	// is no default: the operator chooses the image builds run in.
	Image string
	// Runtime is the OCI runtime for build sandboxes.
	Runtime string
	// Snapshotter is the containerd snapshotter for sandbox roots.
	Snapshotter string
	// CNIPluginDir and CNIConfDir locate the CNI plugins and the
	// build network configuration. The conf dir must contain exactly
	// the CNINetwork: build sandboxes never join the workload mesh.
	CNIPluginDir string
	CNIConfDir   string
	CNINetwork   string
	// Nameservers overrides the resolver configuration rendered
	// into sandboxes. Empty inherits the builder host's
	// non-loopback nameservers; loopback entries never survive the
	// copy because a private network namespace cannot reach the
	// host's loopback resolver.
	Nameservers []string
	// BuildkitdBinary is the BuildKit daemon the hardened executor
	// starts per execution. Each build gets a fresh daemon with an
	// isolated root and socket so sibling builds share no cache,
	// worker, or session state.
	BuildkitdBinary string
}

type MeshRuntimeConfig struct {
	NodeName             string
	Host                 HostConfig
	Containerd           ContainerdConfig
	WireGuard            WireGuard
	Firewall             FirewallConfig
	WorkloadPoolCIDR     string
	WorkloadIPv4PoolCIDR string
}

type HostConfig struct {
	IPv6 string
}

type ContainerdConfig struct {
	Socket            string
	Namespace         string
	EnvironmentLabel  string
	IPv6Label         string
	IPv4Label         string
	IdentitySeeds     []IdentitySeed
	StaticAssignments []ContainerAssignment
}

func (cfg ContainerdConfig) LabelKeys() meshlabels.Keys {
	return meshlabels.NewKeys(cfg.EnvironmentLabel, cfg.IPv4Label, cfg.IPv6Label)
}

type IdentitySeed struct {
	IPv4            string
	IPv6            string
	HostIPv6        string
	NetworkIdentity uint32
}

type ContainerAssignment struct {
	ContainerID     string
	NetworkIdentity uint32
	IPv4            string
	IPv6            string
}

type WireGuard struct {
	InterfaceName     string
	PrivateKey        string
	ListenPort        int
	AdvertiseEndpoint string
	Addresses         []string
	Peers             []PeerConfig
}

type PeerConfig struct {
	Name                 string
	PublicKey            string
	Endpoint             string
	AllowedIPs           []string
	PersistentKeepaliveS int
}

type FirewallConfig struct {
	ConntrackInnerEntries  int
	MaxContainers          int
	ClusterIdentityEntries int
}
