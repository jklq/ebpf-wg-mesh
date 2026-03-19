package controlplane

import (
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
)

type projectKind string

const (
	projectKindUser    projectKind = "user"
	projectKindManaged projectKind = "managed"
)

type principalRecord struct {
	Subject   string
	Email     string
	CreatedAt time.Time
}

type projectRecord struct {
	ID        string
	Name      string
	Kind      projectKind
	SystemKey string
	CreatedAt time.Time
}

type volumeRecord struct {
	ID           string
	ProjectID    string
	Name         string
	SizeBytes    int64
	BoundAgentID string
	CreatedAt    time.Time
}

type serviceRecord struct {
	ID                string
	ProjectID         string
	Name              string
	Spec              *platformv1.ServiceSpec
	SpecRevision      int64
	RolloutGeneration int64
	AllocatedAgentID  string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type domainBindingRecord struct {
	Hostname  string
	ProjectID string
	ServiceID string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type agentRecord struct {
	ID                     string
	Name                   string
	AdvertiseAddr          string
	WorkloadIPv6Subnet     string
	WireGuardPublicKey     string
	WireGuardListenPort    int
	WireGuardIPv6          string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	LastSeenAt             time.Time
}

func (a agentRecord) healthy(now time.Time) bool {
	return now.Sub(a.LastSeenAt) < 30*time.Second
}

type allocationRecord struct {
	ID                       string
	ServiceID                string
	ProjectID                string
	AgentID                  string
	DesiredSpecRevision      int64
	AppliedSpecRevision      int64
	DesiredRolloutGeneration int64
	AppliedRolloutGeneration int64
	Phase                    string
	Message                  string
	EndpointAddr             string
	Healthy                  bool
	UpdatedAt                time.Time
}
