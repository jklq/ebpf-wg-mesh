package config

type ServerTLSConfig struct {
	ServerNames             []string
	BootstrapTokens         []string
	ServerCertValidityHours int
	ClientCertValidityHours int
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
	URL string
}

type OIDCConfig struct {
	Issuer             string
	Audience           string
	JWKSURL            string
	AllowedEmailDomain string
}

type BootstrapConfig struct {
	Users []BootstrapUser
}

type BootstrapUser struct {
	Subject  string
	Email    string
	Projects []string
}

type IngressConfig struct {
	AdminURL                 string
	PublicAddr               string
	ControlPlaneHTTPUpstream string
}

type ManagedDashboardConfig struct {
	Enabled           bool
	ProjectName       string
	ProjectSystemKey  string
	ServiceName       string
	ServiceCallerID   string
	PublicDomain      string
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

type ControlPlaneConfig struct {
	PublicHTTP   ListenerConfig
	InternalGRPC ListenerConfig
	Database     DatabaseConfig
	StateDir     string
	OIDC         OIDCConfig
	Ingress      IngressConfig
	Dashboard    ManagedDashboardConfig
	Bootstrap    BootstrapConfig
	Mesh         ControlPlaneMeshConfig
}

type ControlPlaneMeshConfig struct {
	InterfaceName              string
	ListenPort                 int
	NetworkCIDR                string
	WorkloadPoolCIDR           string
	PersistentKeepaliveSeconds int
}

type NodeConfig struct {
	ID            string
	Name          string
	AdvertiseAddr string
	Resources     NodeResourcesConfig
}

type NodeResourcesConfig struct {
	CPUMillis       int64
	MemoryMebibytes int64
}

type ControlPlaneClientConfig struct {
	Address string
	TLS     ClientTLSConfig
}

type RuntimeConfig struct {
	DataDir        string
	VolumesDir     string
	Snapshotter    string
	DisableCgroups bool
}

type MeshConfig struct {
	Host      HostConfig
	WireGuard WireGuard
	Firewall  FirewallConfig
}

type AgentConfig struct {
	Node         NodeConfig
	ControlPlane ControlPlaneClientConfig
	Runtime      RuntimeConfig
	Containerd   ContainerdConfig
	Mesh         MeshConfig
}

type AgentMeshAssignment struct {
	WorkloadIPv6Subnet string
	WireGuardAddresses []string
	Peers              []PeerConfig
}

type MeshRuntimeConfig struct {
	NodeName   string
	Host       HostConfig
	Containerd ContainerdConfig
	WireGuard  WireGuard
	Firewall   FirewallConfig
}

func (cfg AgentConfig) MeshRuntimeConfig(assignment AgentMeshAssignment) MeshRuntimeConfig {
	wireGuard := cfg.Mesh.WireGuard
	wireGuard.Addresses = append([]string(nil), assignment.WireGuardAddresses...)
	wireGuard.Peers = append([]PeerConfig(nil), assignment.Peers...)
	return MeshRuntimeConfig{
		NodeName:   cfg.Node.Name,
		Host:       cfg.Mesh.Host,
		Containerd: cfg.Containerd,
		WireGuard:  wireGuard,
		Firewall:   cfg.Mesh.Firewall,
	}
}

type HostConfig struct {
	IPv6 string
}

type ContainerdConfig struct {
	Socket            string
	Namespace         string
	ProjectLabel      string
	IPv6Label         string
	IdentitySeeds     []IdentitySeed
	StaticAssignments []ContainerAssignment
}

type IdentitySeed struct {
	IPv6      string
	HostIPv6  string
	ProjectID uint32
}

type ContainerAssignment struct {
	ContainerID string
	ProjectID   uint32
	IPv6        string
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
