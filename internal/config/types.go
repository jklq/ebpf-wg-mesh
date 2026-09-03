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
	// UseReportedAllocationIP is for runtimes whose workload address is
	// reachable directly by the ingress network (for example local Docker).
	// Production mesh deployments should keep the default false so ingress
	// uses the control-plane-derived workload IPv6 address.
	UseReportedAllocationIP bool
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

type UserAssertionConfig struct {
	HMACSecret string
}

type GitHubAppConfig struct {
	Enabled       bool
	AppID         int64
	WebhookSecret string
	PrivateKeyPEM string
	APIBaseURL    string
	WebBaseURL    string
	WebhookPath   string
}

type RegistryConfig struct {
	Host                 string
	NamespacePrefix      string
	AuthListen           string
	TokenIssuer          string
	TokenService         string
	CredentialTTLSeconds int
	SigningCertFile      string
	SigningKeyFile       string
}

type ControlPlaneBuilderConfig struct {
	HeartbeatTimeoutSeconds int
}

type ControlPlaneFailoverConfig struct {
	ReconcileIntervalSeconds  int
	UnhealthyThresholdSeconds int
}

type SourceArchiveConfig struct {
	Directory     string
	RetentionDays int
}

type ControlPlaneConfig struct {
	Profile        Profile
	Health         HealthConfig
	InternalGRPC   ListenerConfig
	UserAssertions UserAssertionConfig
	Database       DatabaseConfig
	Logs           LogCaptureConfig
	StateDir       string
	SourceArchives SourceArchiveConfig
	Ingress        IngressConfig
	Dashboard      ManagedDashboardConfig
	Bootstrap      BootstrapConfig
	GitHub         GitHubAppConfig
	Registry       RegistryConfig
	Builder        ControlPlaneBuilderConfig
	Failover       ControlPlaneFailoverConfig
	Mesh           ControlPlaneMeshConfig
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
	Address string
	TLS     ClientTLSConfig
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
	GitBinary                string
	BuildctlBinary           string
	BuildkitAddress          string
	CleanupWorkDir           bool
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

// LabelKeys resolves the identity label keys shared by the agent, which stamps
// them, and the firewall, which reads them back.
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
	InterfaceName string
	PrivateKey    string
	ListenPort    int
	Addresses     []string
	Peers         []PeerConfig
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
