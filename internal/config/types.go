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
	AdminURL                 string
	AdminListen              string
	AllowNonLoopbackAdmin    bool
	ListenAddrs              []string
	DisableAutomaticHTTPS    bool
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
	Executor                 string
	BuildctlBinary           string
	BuildkitAddress          string
	RailpackBinary           string
	RailpackFrontendImage    string
	Limits                   BuilderLimitsConfig
	Network                  BuilderNetworkConfig
	CleanupWorkDir           bool
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
