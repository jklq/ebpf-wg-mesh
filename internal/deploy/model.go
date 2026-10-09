// Package deploy owns installations outside the platform's workload scheduler
// and database. Provider identity is deliberately separate from host eligibility.
package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Role string

const (
	Database     Role = "cockroachdb"
	ControlPlane Role = "controlplane"
	Console      Role = "console"
	Envoy        Role = "envoy"
	Registry     Role = "registry"
	Builder      Role = "builder"
	Agent        Role = "agent"
)

var Roles = []Role{Database, ControlPlane, Console, Envoy, Registry, Builder, Agent}

type Resources struct {
	CPUMillis int64 `json:"cpuMillis" yaml:"cpuMillis"`
	MemoryMiB int64 `json:"memoryMiB" yaml:"memoryMiB"`
	DiskGiB   int64 `json:"diskGiB" yaml:"diskGiB"`
}

func (r Resources) Add(v Resources) Resources {
	return Resources{r.CPUMillis + v.CPUMillis, r.MemoryMiB + v.MemoryMiB, r.DiskGiB + v.DiskGiB}
}
func (r Resources) Sub(v Resources) Resources {
	return Resources{r.CPUMillis - v.CPUMillis, r.MemoryMiB - v.MemoryMiB, r.DiskGiB - v.DiskGiB}
}
func (r Resources) Fits(v Resources) bool {
	return r.CPUMillis >= v.CPUMillis && r.MemoryMiB >= v.MemoryMiB && r.DiskGiB >= v.DiskGiB
}
func (r Resources) valid() bool { return r.CPUMillis >= 0 && r.MemoryMiB >= 0 && r.DiskGiB >= 0 }

// Secret values never appear in manifests or plans. The local file store is
// independent of platform authentication and can be populated by a vault agent.
type SecretRef struct {
	File string `json:"file" yaml:"file"` // component references support instance/host substitutions in the path
}
type Provider struct {
	Kind  string `json:"kind" yaml:"kind"`
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
}
type Binding struct {
	Provider string `json:"provider" yaml:"provider"`
	ServerID string `json:"serverId,omitempty" yaml:"serverId,omitempty"`
}
type Purchase struct {
	ServerType  string   `json:"serverType" yaml:"serverType"`
	Location    string   `json:"location" yaml:"location"`
	Image       string   `json:"image" yaml:"image"`
	SSHKeys     []string `json:"sshKeys" yaml:"sshKeys"`
	MonthlyCost string   `json:"monthlyCost,omitempty" yaml:"monthlyCost,omitempty"`
	Currency    string   `json:"currency,omitempty" yaml:"currency,omitempty"`
}
type SSH struct {
	Address    string `json:"address" yaml:"address"`
	User       string `json:"user" yaml:"user"`
	Key        string `json:"key" yaml:"key"`
	KnownHosts string `json:"knownHosts" yaml:"knownHosts"`
}
type Network struct {
	Address string `json:"address" yaml:"address"`
	Public  bool   `json:"public" yaml:"public"`
	// NAT hosts expose SSH through an established outbound reverse tunnel on
	// one of these gateways. Address is the tunnel endpoint, not a private IP.
	Gateways []string       `json:"gateways,omitempty" yaml:"gateways,omitempty"`
	Peers    map[string]int `json:"peers,omitempty" yaml:"peers,omitempty"` // round-trip milliseconds
}

// SocketHost formats a host for a URL authority or a host:port argument while
// Address remains the bare IP/DNS identity used in certificates and the mesh.
func (n Network) SocketHost() string {
	if ip := net.ParseIP(n.Address); ip != nil && ip.To4() == nil {
		return "[" + n.Address + "]"
	}
	return n.Address
}

type Host struct {
	ID            string    `json:"id" yaml:"id"`
	Binding       Binding   `json:"binding" yaml:"binding"`
	Purchase      *Purchase `json:"purchase,omitempty" yaml:"purchase,omitempty"`
	SSH           SSH       `json:"ssh" yaml:"ssh"`
	Architecture  string    `json:"architecture" yaml:"architecture"`
	Capacity      Resources `json:"capacity" yaml:"capacity"`
	Reserve       Resources `json:"reserve" yaml:"reserve"`
	DiskClass     string    `json:"diskClass" yaml:"diskClass"`
	Capabilities  []string  `json:"capabilities" yaml:"capabilities"`
	Reliability   string    `json:"reliability" yaml:"reliability"`
	Trusted       bool      `json:"trusted" yaml:"trusted"`
	Roles         []Role    `json:"roles" yaml:"roles"`
	FailureDomain string    `json:"failureDomain" yaml:"failureDomain"`
	Region        string    `json:"region" yaml:"region"`
	Network       Network   `json:"network" yaml:"network"`
}
type Component struct {
	SecretEnv        map[string]string `json:"secretEnv,omitempty" yaml:"secretEnv,omitempty"`
	Replicas         int               `json:"replicas" yaml:"replicas"`
	ReliableReplicas int               `json:"reliableReplicas" yaml:"reliableReplicas"`
	Resources        Resources         `json:"resources" yaml:"resources"`
	Hosts            []string          `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Capabilities     []string          `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	DiskClass        string            `json:"diskClass,omitempty" yaml:"diskClass,omitempty"`
	DistinctDomains  bool              `json:"distinctDomains" yaml:"distinctDomains"`
	Storage          []string          `json:"storage,omitempty" yaml:"storage,omitempty"`
	Secrets          map[string]string `json:"secrets,omitempty" yaml:"secrets,omitempty"` // relative destination -> reference
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}
type Storage struct {
	Path       string   `json:"path" yaml:"path"`
	Hosts      []string `json:"hosts" yaml:"hosts"`
	Replicated bool     `json:"replicated" yaml:"replicated"`
}
type Endpoint struct {
	Name     string   `json:"name" yaml:"name"`
	URL      string   `json:"url" yaml:"url"`
	Role     Role     `json:"role" yaml:"role"`
	Hosts    []string `json:"hosts" yaml:"hosts"`
	Gateways []string `json:"gateways,omitempty" yaml:"gateways,omitempty"`
}
type Backup struct {
	Target         string   `json:"target" yaml:"target"`
	Credentials    []string `json:"credentials" yaml:"credentials"`
	Account        string   `json:"account" yaml:"account"`
	PrimaryAccount string   `json:"primaryAccount" yaml:"primaryAccount"`
	FailureDomain  string   `json:"failureDomain" yaml:"failureDomain"`
	RecoveryKey    string   `json:"recoveryKey" yaml:"recoveryKey"`
	Monitor        string   `json:"monitor" yaml:"monitor"`
}
type Recovery struct {
	Inventory   string   `json:"inventory" yaml:"inventory"`
	Hosts       []Host   `json:"hosts" yaml:"hosts"`
	Credentials []string `json:"credentials" yaml:"credentials"`
}
type Installation struct {
	// OperationsInputs are private service-selection files copied from the operator
	// workspace to the administration host. Generated runtime secrets stay remote.
	OperationsInputs     []string             `json:"operationsInputs,omitempty" yaml:"operationsInputs,omitempty"`
	OperationsConfig     string               `json:"operationsConfig,omitempty" yaml:"operationsConfig,omitempty"`
	RetireHosts          []string             `json:"retireHosts,omitempty" yaml:"retireHosts,omitempty"`
	Version              int                  `json:"version" yaml:"version"`
	ID                   string               `json:"id" yaml:"id"`
	Release              string               `json:"release" yaml:"release"`
	ManagementHost       string               `json:"managementHost" yaml:"managementHost"`
	Providers            map[string]Provider  `json:"providers" yaml:"providers"`
	Secrets              map[string]SecretRef `json:"secrets" yaml:"secrets"`
	Hosts                []Host               `json:"hosts" yaml:"hosts"`
	Components           map[Role]Component   `json:"components" yaml:"components"`
	Storage              map[string]Storage   `json:"storage" yaml:"storage"`
	Endpoints            []Endpoint           `json:"endpoints" yaml:"endpoints"`
	Backup               Backup               `json:"backup" yaml:"backup"`
	Recovery             Recovery             `json:"recovery" yaml:"recovery"`
	OneHostFailure       bool                 `json:"oneHostFailure" yaml:"oneHostFailure"`
	Workload             Resources            `json:"workload" yaml:"workload"`
	MaxDatabaseRTTMillis int                  `json:"maxDatabaseRttMillis" yaml:"maxDatabaseRttMillis"`
}

// A release contains executable artifacts, never floating package tags. Hooks
// are release-owned, idempotent commands with independent observation commands.
// They bridge native database/admin protocols without adding a second scheduler.
type Artifact struct {
	URL    string `json:"url" yaml:"url"`
	SHA256 string `json:"sha256" yaml:"sha256"`
}
type Hook struct {
	Command []string `json:"command" yaml:"command"`
	Verify  []string `json:"verify" yaml:"verify"`
}
type Program struct {
	Artifacts map[string]Artifact `json:"artifacts" yaml:"artifacts"`
	Args      []string            `json:"args" yaml:"args"`
	Ready     []string            `json:"ready" yaml:"ready"`
	Drain     Hook                `json:"drain" yaml:"drain"`
	Retire    Hook                `json:"retire" yaml:"retire"`
}
type Release struct {
	Tools         map[string]map[string]Artifact `json:"tools,omitempty" yaml:"tools,omitempty"`
	Version       int                            `json:"version" yaml:"version"`
	ID            string                         `json:"id" yaml:"id"`
	Configuration int                            `json:"configuration" yaml:"configuration"`
	Protocol      int                            `json:"protocol" yaml:"protocol"`
	Schema        int                            `json:"schema" yaml:"schema"`
	ConsoleSchema int                            `json:"consoleSchema" yaml:"consoleSchema"`
	Dependencies  map[string]string              `json:"dependencies" yaml:"dependencies"`
	Images        map[string]string              `json:"images" yaml:"images"`
	Programs      map[Role]Program               `json:"programs" yaml:"programs"`
	Hooks         map[string]Hook                `json:"hooks" yaml:"hooks"`
	// Direct, explicit conversion from each supported source release. There is
	// no migration chain. Restore is the only rollback across a schema cutover.
	Conversions map[string]Hook `json:"conversions,omitempty" yaml:"conversions,omitempty"`
}

func Load[T any](path string) (T, error) {
	var value T
	f, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return value, err
	}
	if len(data) > 4<<20 {
		return value, fmt.Errorf("%s exceeds the document size limit", path)
	}
	var d interface{ Decode(any) error }
	if json.Valid(data) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		d = decoder
	} else {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		d = decoder
	}
	if err := d.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return value, fmt.Errorf("%s must contain exactly one document", path)
	}
	return value, nil
}
func Digest(v any) string {
	b, _ := json.Marshal(v)
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func snapshot[T any](value T) T {
	raw, _ := json.Marshal(value)
	var copy T
	_ = json.Unmarshal(raw, &copy)
	return copy
}
func (i Installation) Host(id string) (Host, bool) {
	for _, h := range i.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}
func (i Installation) Resolve(ref string) ([]byte, error) {
	s, ok := i.Secrets[ref]
	if !ok {
		return nil, fmt.Errorf("unknown secret reference %q", ref)
	}
	return readSecretFile(ref, s.File)
}

func readSecretFile(ref, file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("read secret %s: %w", ref, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secret %s must be a private regular file", ref)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read secret %s: %w", ref, err)
	}
	return b, nil
}
func contains[T comparable](vs []T, v T) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}
func safeID(s string) bool {
	if s == "" || len(s) > 63 || strings.Contains(s, "..") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return s[0] != '-'
}
