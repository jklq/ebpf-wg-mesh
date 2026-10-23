// Package authz is the single owner of user authorization in the control plane. It
// mints unforgeable scope values proving a dashboard user may access a workspace,
// project, environment, service, volume, domain binding, or the operator fleet surface.
//
// Workspace membership is the only user grant: a member's workspace role applies to
// every project in that workspace.
//
// The entry pattern is: request-adjacent methods take an authz.User and mint the scope
// they need first; everything downstream takes the scope and derives resource IDs from it
// instead of trusting caller-supplied IDs. A missing authorization is therefore a compile
// error or a denial, never silent access.
//
// Scopes are minted on the shared database handle rather than inside product transactions.
// Every Authorize call reads live membership (no caching), so a role change takes effect
// on the very next call; the remaining mint-to-commit window is a single statement,
// pinned by TestAuthorizerReadsLiveMembership.
package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Access is the membership level an operation requires.
type Access int

const (
	// Read allows viewer, member, and admin roles.
	Read Access = iota + 1
	// Write allows member and admin roles.
	Write
	// Manage allows the admin role: workspace settings, members, and invitations.
	Manage
	// ReadSecrets allows member and admin roles to reveal secret values
	// (service env). It is distinct from Read so a viewer who may inspect a
	// service never receives its secrets; it matches Write's roles because a
	// writer can already deploy code that prints its own env.
	ReadSecrets
)

// Role is a workspace membership role.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// ParseRole validates a stored or requested role name.
func ParseRole(value string) (Role, error) {
	switch role := Role(value); role {
	case RoleAdmin, RoleMember, RoleViewer:
		return role, nil
	default:
		return "", fmt.Errorf("authz: unknown role %q", value)
	}
}

// Allows reports whether the role satisfies the required access.
func (r Role) Allows(need Access) bool {
	switch need {
	case Read:
		return r == RoleAdmin || r == RoleMember || r == RoleViewer
	case Write, ReadSecrets:
		return r == RoleAdmin || r == RoleMember
	case Manage:
		return r == RoleAdmin
	default:
		return false
	}
}

// UserProjectKind is the kind column value for user projects. Managed projects are never
// user-accessible, so every scope check constrains to it; product code aliases this single definition.
const UserProjectKind = "user"

// ErrDenied reports an authorization failure. Denials also wrap sql.ErrNoRows so existing
// not-found/denied mappings keep working: a denial is indistinguishable from a missing resource by design.
var ErrDenied = errors.New("authz: access denied")

func deny(op string) error {
	return fmt.Errorf("%w: %s: %w", ErrDenied, op, sql.ErrNoRows)
}

// User is an authenticated dashboard user, minted at the RPC boundary with AuthenticatedUser.
type User struct{ id string }

// AuthenticatedUser validates a delegated user ID from context, rejecting empty IDs.
func AuthenticatedUser(id string) (User, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return User{}, errors.New("authz: authenticated user id is required")
	}
	return User{id: id}, nil
}

func (u User) ID() string { return u.id }

// Operator proves platform-operator membership.
type Operator struct{ user User }

func (o Operator) UserID() string { return o.user.id }

func (o Operator) User() User { return o.user }

// Workspace proves membership in a workspace with at least the minted access.
type Workspace struct {
	user User
	id   string
	role Role
}

func (w Workspace) ID() string { return w.id }

func (w Workspace) UserID() string { return w.user.id }

func (w Workspace) User() User { return w.user }

func (w Workspace) Role() Role { return w.role }

// Project proves workspace membership for a user project with at least the minted access.
type Project struct {
	user        User
	id          string
	workspaceID string
	role        Role
}

func (p Project) ID() string { return p.id }

// WorkspaceID is the workspace that owns the project. Billing derives its
// payer from this scope instead of trusting a caller-supplied account.
func (p Project) WorkspaceID() string { return p.workspaceID }

func (p Project) UserID() string { return p.user.id }

func (p Project) User() User { return p.user }

func (p Project) Role() Role { return p.role }

// Environment proves membership in an environment's project.
type Environment struct {
	project Project
	id      string
}

func (e Environment) ID() string { return e.id }

func (e Environment) ProjectID() string { return e.project.id }

func (e Environment) WorkspaceID() string { return e.project.workspaceID }

func (e Environment) Project() Project { return e.project }

func (e Environment) UserID() string { return e.project.user.id }

func (e Environment) User() User { return e.project.user }

func (e Environment) Role() Role { return e.project.role }

// Service proves membership in a service's project.
type Service struct {
	environment Environment
	id          string
}

func (s Service) ID() string { return s.id }

func (s Service) EnvironmentID() string { return s.environment.id }

func (s Service) Environment() Environment { return s.environment }

func (s Service) ProjectID() string { return s.environment.project.id }

func (s Service) WorkspaceID() string { return s.environment.project.workspaceID }

func (s Service) Project() Project { return s.environment.project }

func (s Service) UserID() string { return s.environment.project.user.id }

func (s Service) User() User { return s.environment.project.user }

func (s Service) Role() Role { return s.environment.project.role }

// Volume proves membership in a volume's project.
type Volume struct {
	environment Environment
	id          string
}

func (v Volume) ID() string { return v.id }

func (v Volume) EnvironmentID() string { return v.environment.id }

func (v Volume) Environment() Environment { return v.environment }

func (v Volume) ProjectID() string { return v.environment.project.id }

func (v Volume) WorkspaceID() string { return v.environment.project.workspaceID }

func (v Volume) UserID() string { return v.environment.project.user.id }

func (v Volume) User() User { return v.environment.project.user }

func (v Volume) Role() Role { return v.environment.project.role }

// DomainBinding proves membership in a domain binding's project.
type DomainBinding struct {
	service  Service
	hostname string
}

func (b DomainBinding) Hostname() string { return b.hostname }

func (b DomainBinding) ServiceID() string { return b.service.id }

func (b DomainBinding) Service() Service { return b.service }

func (b DomainBinding) EnvironmentID() string { return b.service.environment.id }

func (b DomainBinding) ProjectID() string { return b.service.environment.project.id }

func (b DomainBinding) WorkspaceID() string { return b.service.environment.project.workspaceID }

func (b DomainBinding) UserID() string { return b.service.environment.project.user.id }

func (b DomainBinding) User() User { return b.service.environment.project.user }

func (b DomainBinding) Role() Role { return b.service.environment.project.role }

// Querier is the minimal read surface authorization needs; both *sql.DB and *sql.Tx satisfy it.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Authorizer mints scopes against its querier. Construct one per database handle and share it.
type Authorizer struct{ q Querier }

// NewAuthorizer returns an Authorizer reading membership from q; it panics on a nil querier.
func NewAuthorizer(q Querier) *Authorizer {
	if q == nil {
		panic("authz: nil querier")
	}
	return &Authorizer{q: q}
}

func validNeed(need Access) bool {
	return need == Read || need == Write || need == Manage || need == ReadSecrets
}

// check validates the Authorize inputs that must never reach SQL: an empty user ID, and
// an invalid need (a caller bug that Allows would otherwise hide as a denial).
func check(user User, need Access) error {
	if user.id == "" {
		return errors.New("authz: user id is required")
	}
	if !validNeed(need) {
		return fmt.Errorf("authz: invalid access %d", need)
	}
	return nil
}

// AuthorizeOperator proves user is a platform operator.
func (a *Authorizer) AuthorizeOperator(ctx context.Context, user User) (Operator, error) {
	if user.id == "" {
		return Operator{}, errors.New("authz: user id is required")
	}
	var one int
	if err := a.q.QueryRowContext(ctx, `SELECT 1 FROM platform_operators WHERE user_id = $1`, user.id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operator{}, deny("operator")
		}
		return Operator{}, err
	}
	return Operator{user: user}, nil
}

// AuthorizeWorkspace proves user holds at least need access on a workspace.
func (a *Authorizer) AuthorizeWorkspace(ctx context.Context, user User, workspaceID string, need Access) (Workspace, error) {
	if err := check(user, need); err != nil {
		return Workspace{}, err
	}
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT role FROM workspace_memberships
		 WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, user.id).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workspace{}, deny("workspace")
		}
		return Workspace{}, err
	}
	if !role.Allows(need) {
		return Workspace{}, deny("workspace")
	}
	return Workspace{user: user, id: workspaceID, role: role}, nil
}

// AuthorizeProject proves user holds at least need access on a user project.
func (a *Authorizer) AuthorizeProject(ctx context.Context, user User, projectID string, need Access) (Project, error) {
	if err := check(user, need); err != nil {
		return Project{}, err
	}
	var role Role
	var workspaceID string
	if err := a.q.QueryRowContext(ctx, `SELECT m.role, p.workspace_id
		  FROM projects p
		  JOIN workspace_memberships m ON m.workspace_id = p.workspace_id
		 WHERE p.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		projectID, user.id, UserProjectKind).Scan(&role, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, deny("project")
		}
		return Project{}, err
	}
	if !role.Allows(need) {
		return Project{}, deny("project")
	}
	return Project{user: user, id: projectID, workspaceID: workspaceID, role: role}, nil
}

// AuthorizeEnvironment proves user holds at least need access on the environment's project.
func (a *Authorizer) AuthorizeEnvironment(ctx context.Context, user User, environmentID string, need Access) (Environment, error) {
	if err := check(user, need); err != nil {
		return Environment{}, err
	}
	var projectID, workspaceID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT e.project_id, m.role, p.workspace_id
		  FROM environments e
		  JOIN projects p ON p.id = e.project_id
		  JOIN workspace_memberships m ON m.workspace_id = p.workspace_id
		 WHERE e.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		environmentID, user.id, UserProjectKind).Scan(&projectID, &role, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Environment{}, deny("environment")
		}
		return Environment{}, err
	}
	if !role.Allows(need) {
		return Environment{}, deny("environment")
	}
	return Environment{project: Project{user: user, id: projectID, workspaceID: workspaceID, role: role}, id: environmentID}, nil
}

// AuthorizeService proves user holds at least need access on the service's project.
func (a *Authorizer) AuthorizeService(ctx context.Context, user User, serviceID string, need Access) (Service, error) {
	if err := check(user, need); err != nil {
		return Service{}, err
	}
	var environmentID, projectID, workspaceID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT s.environment_id, e.project_id, m.role, p.workspace_id
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN workspace_memberships m ON m.workspace_id = p.workspace_id
		 WHERE s.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		serviceID, user.id, UserProjectKind).Scan(&environmentID, &projectID, &role, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Service{}, deny("service")
		}
		return Service{}, err
	}
	if !role.Allows(need) {
		return Service{}, deny("service")
	}
	project := Project{user: user, id: projectID, workspaceID: workspaceID, role: role}
	return Service{environment: Environment{project: project, id: environmentID}, id: serviceID}, nil
}

// AuthorizeVolume proves user holds at least need access on the volume's project.
func (a *Authorizer) AuthorizeVolume(ctx context.Context, user User, volumeID string, need Access) (Volume, error) {
	if err := check(user, need); err != nil {
		return Volume{}, err
	}
	var environmentID, projectID, workspaceID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT v.environment_id, e.project_id, m.role, p.workspace_id
		  FROM volumes v
		  JOIN environments e ON e.id = v.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN workspace_memberships m ON m.workspace_id = p.workspace_id
		 WHERE v.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		volumeID, user.id, UserProjectKind).Scan(&environmentID, &projectID, &role, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Volume{}, deny("volume")
		}
		return Volume{}, err
	}
	if !role.Allows(need) {
		return Volume{}, deny("volume")
	}
	project := Project{user: user, id: projectID, workspaceID: workspaceID, role: role}
	return Volume{environment: Environment{project: project, id: environmentID}, id: volumeID}, nil
}

// AuthorizeDomainBinding proves user holds at least need access on the binding's project.
func (a *Authorizer) AuthorizeDomainBinding(ctx context.Context, user User, hostname string, need Access) (DomainBinding, error) {
	if err := check(user, need); err != nil {
		return DomainBinding{}, err
	}
	// Normalize like the routing layer so "Example.COM." authorizes the same binding it routes to.
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	var serviceID, environmentID, projectID, workspaceID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT d.service_id, s.environment_id, e.project_id, m.role, p.workspace_id
		  FROM domain_bindings d
		  JOIN services s ON s.id = d.service_id
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN workspace_memberships m ON m.workspace_id = p.workspace_id
		 WHERE d.hostname = $1 AND m.user_id = $2 AND p.kind = $3`,
		hostname, user.id, UserProjectKind).Scan(&serviceID, &environmentID, &projectID, &role, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DomainBinding{}, deny("domain binding")
		}
		return DomainBinding{}, err
	}
	if !role.Allows(need) {
		return DomainBinding{}, deny("domain binding")
	}
	project := Project{user: user, id: projectID, workspaceID: workspaceID, role: role}
	service := Service{environment: Environment{project: project, id: environmentID}, id: serviceID}
	return DomainBinding{service: service, hostname: hostname}, nil
}
