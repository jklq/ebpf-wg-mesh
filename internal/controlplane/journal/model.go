package journal

import (
	"encoding/json"
	"time"
)

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	SystemKey   *string   `json:"system_key"`
	OwnerUserID string    `json:"owner_user_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type ServiceIntent struct {
	ID                       string    `json:"id"`
	EnvironmentID            string    `json:"environment_id"`
	Name                     string    `json:"name"`
	CurrentSpecRevision      int64     `json:"current_spec_revision"`
	CurrentRolloutGeneration int64     `json:"current_rollout_generation"`
	CurrentArtifactID        string    `json:"current_artifact_id"`
	CurrentResolvedImage     string    `json:"current_resolved_image"`
	LastSuccessfulCommitSHA  string    `json:"last_successful_commit_sha"`
	LatestBuildID            string    `json:"latest_build_id"`
	DesiredReplicaCount      int64     `json:"desired_replica_count"`
	PlacementMessage         string    `json:"placement_message"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type ServiceRevision struct {
	ServiceID    string          `json:"service_id"`
	SpecRevision int64           `json:"spec_revision"`
	SpecJSON     json.RawMessage `json:"spec_json"`
	CreatedAt    time.Time       `json:"created_at"`
}

type Assignment struct {
	ID                       string     `json:"id"`
	ServiceID                string     `json:"service_id"`
	DeploymentID             string     `json:"deployment_id"`
	AgentID                  string     `json:"agent_id"`
	DesiredSpecRevision      int64      `json:"desired_spec_revision"`
	DesiredRolloutGeneration int64      `json:"desired_rollout_generation"`
	AllocationIPv4           string     `json:"allocation_ipv4"`
	AllocationIPv6           string     `json:"allocation_ipv6"`
	OperatorRestartNonce     int64      `json:"operator_restart_nonce"`
	RolloutState             string     `json:"rollout_state"`
	Intent                   string     `json:"intent"`
	IntentMessage            string     `json:"intent_message"`
	DrainStartedAt           *time.Time `json:"drain_started_at"`
	DrainDeadline            *time.Time `json:"drain_deadline"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

type Rollout struct {
	ServiceID           string          `json:"service_id"`
	RolloutGeneration   int64           `json:"rollout_generation"`
	SpecRevision        int64           `json:"spec_revision"`
	Reason              string          `json:"reason"`
	BuildID             string          `json:"build_id"`
	RequestedByUserID   string          `json:"requested_by_user_id"`
	State               string          `json:"state"`
	StrategyJSON        json.RawMessage `json:"strategy_json"`
	DesiredReplicaCount int64           `json:"desired_replica_count"`
	ArtifactID          string          `json:"artifact_id"`
	ImageDigest         string          `json:"image_digest"`
	FailureReason       string          `json:"failure_reason"`
	TargetAllocationID  string          `json:"target_allocation_id"`
	CompletedAt         *time.Time      `json:"completed_at"`
	ProgressAt          time.Time       `json:"progress_at"`
	CreatedAt           time.Time       `json:"created_at"`
}

type Deployment struct {
	ID                   string          `json:"id"`
	ServiceID            string          `json:"service_id"`
	SpecRevision         int64           `json:"spec_revision"`
	RolloutGeneration    int64           `json:"rollout_generation"`
	BuildID              string          `json:"build_id"`
	ArtifactID           string          `json:"artifact_id"`
	ImageDigest          string          `json:"image_digest"`
	State                string          `json:"state"`
	CauseKind            string          `json:"cause_kind"`
	CauseID              string          `json:"cause_id"`
	ReasonCode           string          `json:"reason_code"`
	Detail               string          `json:"detail"`
	ResolvedSpecJSON     json.RawMessage `json:"resolved_spec_json"`
	VariableVersionsJSON json.RawMessage `json:"variable_versions_json"`
	SealedVersionsJSON   json.RawMessage `json:"sealed_versions_json"`
	IsCurrent            bool            `json:"is_current"`
	RequestedByUserID    string          `json:"requested_by_user_id"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type AgentRegistration struct {
	ID                      string          `json:"id"`
	Name                    string          `json:"name"`
	LocalStoreID            string          `json:"local_store_id"`
	SessionIncarnation      int64           `json:"session_incarnation"`
	Region                  string          `json:"region"`
	Zone                    string          `json:"zone"`
	FailureDomain           string          `json:"failure_domain"`
	ReservedCPUMillis       int64           `json:"reserved_cpu_millis"`
	ReservedMemoryMebibytes int64           `json:"reserved_memory_mebibytes"`
	AdvertiseAddr           string          `json:"advertise_addr"`
	WorkloadIPv4Subnet      string          `json:"workload_ipv4_subnet"`
	WorkloadIPv6Subnet      string          `json:"workload_ipv6_subnet"`
	WireguardPublicKey      string          `json:"wireguard_public_key"`
	WireguardListenPort     int64           `json:"wireguard_listen_port"`
	WireguardEndpoint       string          `json:"wireguard_endpoint"`
	WireguardIPv6           string          `json:"wireguard_ipv6"`
	CPUMillisCapacity       int64           `json:"cpu_millis_capacity"`
	MemoryMebibytesCapacity int64           `json:"memory_mebibytes_capacity"`
	RuntimeCapabilities     json.RawMessage `json:"runtime_capabilities"`
	SoftwareVersion         string          `json:"software_version"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	DesiredRevision         int64           `json:"desired_revision"`
}

type AgentAdministration struct {
	AgentID             string     `json:"agent_id"`
	LifecycleState      string     `json:"lifecycle_state"`
	OperatorIntent      string     `json:"operator_intent"`
	MaintenanceMessage  string     `json:"maintenance_message"`
	CredentialRevokedAt *time.Time `json:"credential_revoked_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type Environment struct {
	ID                      string    `json:"id"`
	ProjectID               string    `json:"project_id"`
	Name                    string    `json:"name"`
	Kind                    string    `json:"kind"`
	IsProduction            bool      `json:"is_production"`
	AutoDeploy              bool      `json:"auto_deploy"`
	NetworkIdentity         int64     `json:"network_identity"`
	CopiedFromEnvironmentID *string   `json:"copied_from_environment_id"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type Volume struct {
	ID            string    `json:"id"`
	EnvironmentID string    `json:"environment_id"`
	Name          string    `json:"name"`
	SizeBytes     int64     `json:"size_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

type Domain struct {
	Hostname          string    `json:"hostname"`
	ServiceID         string    `json:"service_id"`
	TargetPort        int64     `json:"target_port"`
	PlatformGenerated bool      `json:"platform_generated"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type DurableState struct {
	Projects       map[string]Project
	ClusterID      string
	LogIndex       int64
	Services       map[string]ServiceIntent
	Revisions      map[string]ServiceRevision
	Assignments    map[string]Assignment
	Rollouts       map[string]Rollout
	Deployments    map[string]Deployment
	Agents         map[string]AgentRegistration
	Administration map[string]AgentAdministration
	Environments   map[string]Environment
	Volumes        map[string]Volume
	Domains        map[string]Domain
}

type Change[T any] struct {
	Key   string `json:"key"`
	Value *T     `json:"value"`
}

type Batch struct {
	Projects       []Change[Project]             `json:"projects,omitempty"`
	BaseIndex      int64                         `json:"base_index"`
	Services       []Change[ServiceIntent]       `json:"services,omitempty"`
	Revisions      []Change[ServiceRevision]     `json:"revisions,omitempty"`
	Assignments    []Change[Assignment]          `json:"assignments,omitempty"`
	Rollouts       []Change[Rollout]             `json:"rollouts,omitempty"`
	Deployments    []Change[Deployment]          `json:"deployments,omitempty"`
	Agents         []Change[AgentRegistration]   `json:"agents,omitempty"`
	Administration []Change[AgentAdministration] `json:"administration,omitempty"`
	Environments   []Change[Environment]         `json:"environments,omitempty"`
	Volumes        []Change[Volume]              `json:"volumes,omitempty"`
	Domains        []Change[Domain]              `json:"domains,omitempty"`
}
