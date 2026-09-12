// Package authz is the single owner of user authorization in the control
// plane. It mints unforgeable scope values that prove a dashboard user may
// access a project, environment, service, volume, domain binding, or the
// operator fleet surface.
//
// The entry pattern is: request-adjacent methods take an authz.User and mint
// the scope they need as their first step; everything downstream takes the
// scope and derives resource IDs from it instead of trusting caller-supplied
// IDs. A missing authorization is therefore a compile error (no scope to
// pass) or a denial (zero-value scopes match no rows), never silent access.
//
// Membership in user projects is immutable outside project creation, so
// scopes are minted on the shared database handle rather than inside product
// transactions. Every Authorize call reads live membership (no caching), so a
// role change or removal takes effect on the very next call; the remaining
// mint-to-commit window is a single statement, pinned by
// TestAuthorizerReadsLiveMembership.
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
	// Read allows viewer, editor, and owner roles.
	Read Access = iota + 1
	// Write allows editor and owner roles.
	Write
)

// Role is a project membership role.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Allows reports whether the role satisfies the required access.
func (r Role) Allows(need Access) bool {
	switch need {
	case Read:
		return r == RoleOwner || r == RoleEditor || r == RoleViewer
	case Write:
		return r == RoleOwner || r == RoleEditor
	default:
		return false
	}
}

// UserProjectKind mirrors the kind column value for user projects. Managed
// projects are never user-accessible, so every scope check constrains to it.
// This is the single definition; product code aliases it instead of repeating
// the literal.
const UserProjectKind = "user"

// ErrDenied reports an authorization failure. Denials also wrap
// sql.ErrNoRows so existing not-found/denied error mappings keep working:
// a denial is indistinguishable from a missing resource by design.
var ErrDenied = errors.New("authz: access denied")

func deny(op string) error {
	return fmt.Errorf("%w: %s: %w", ErrDenied, op, sql.ErrNoRows)
}

// User is an authenticated dashboard user. Mint it from the delegated user
// identity at the RPC boundary with AuthenticatedUser.
type User struct{ id string }

// AuthenticatedUser validates a delegated user ID from context. It rejects
// empty IDs so a missing identity can never authorize.
func AuthenticatedUser(id string) (User, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return User{}, errors.New("authz: authenticated user id is required")
	}
	return User{id: id}, nil
}

// ID returns the user ID the scope was minted for.
func (u User) ID() string { return u.id }

// Operator proves platform-operator membership.
type Operator struct{ user User }

// UserID returns the operator's user ID.
func (o Operator) UserID() string { return o.user.id }

// User returns the operator's user identity.
func (o Operator) User() User { return o.user }

// Project proves membership in a user project with at least the access the
// scope was minted for.
type Project struct {
	user User
	id   string
	role Role
}

// ID returns the authorized project ID.
func (p Project) ID() string { return p.id }

// UserID returns the user ID the scope was minted for.
func (p Project) UserID() string { return p.user.id }

// User returns the user identity the scope was minted for.
func (p Project) User() User { return p.user }

// Role returns the membership role the scope was minted with.
func (p Project) Role() Role { return p.role }

// Environment proves membership in an environment's project.
type Environment struct {
	project Project
	id      string
}

// ID returns the authorized environment ID.
func (e Environment) ID() string { return e.id }

// ProjectID returns the environment's project ID.
func (e Environment) ProjectID() string { return e.project.id }

// Project returns the environment's project scope.
func (e Environment) Project() Project { return e.project }

// UserID returns the user ID the scope was minted for.
func (e Environment) UserID() string { return e.project.user.id }

// User returns the user identity the scope was minted for.
func (e Environment) User() User { return e.project.user }

// Role returns the membership role the scope was minted with.
func (e Environment) Role() Role { return e.project.role }

// Service proves membership in a service's project.
type Service struct {
	environment Environment
	id          string
}

// ID returns the authorized service ID.
func (s Service) ID() string { return s.id }

// EnvironmentID returns the service's environment ID.
func (s Service) EnvironmentID() string { return s.environment.id }

// Environment returns the service's environment scope.
func (s Service) Environment() Environment { return s.environment }

// ProjectID returns the service's project ID.
func (s Service) ProjectID() string { return s.environment.project.id }

// Project returns the service's project scope.
func (s Service) Project() Project { return s.environment.project }

// UserID returns the user ID the scope was minted for.
func (s Service) UserID() string { return s.environment.project.user.id }

// User returns the user identity the scope was minted for.
func (s Service) User() User { return s.environment.project.user }

// Role returns the membership role the scope was minted with.
func (s Service) Role() Role { return s.environment.project.role }

// Volume proves membership in a volume's project.
type Volume struct {
	environment Environment
	id          string
}

// ID returns the authorized volume ID.
func (v Volume) ID() string { return v.id }

// EnvironmentID returns the volume's environment ID.
func (v Volume) EnvironmentID() string { return v.environment.id }

// Environment returns the volume's environment scope.
func (v Volume) Environment() Environment { return v.environment }

// ProjectID returns the volume's project ID.
func (v Volume) ProjectID() string { return v.environment.project.id }

// UserID returns the user ID the scope was minted for.
func (v Volume) UserID() string { return v.environment.project.user.id }

// User returns the user identity the scope was minted for.
func (v Volume) User() User { return v.environment.project.user }

// Role returns the membership role the scope was minted with.
func (v Volume) Role() Role { return v.environment.project.role }

// DomainBinding proves membership in a domain binding's project.
type DomainBinding struct {
	service  Service
	hostname string
}

// Hostname returns the authorized binding hostname.
func (b DomainBinding) Hostname() string { return b.hostname }

// ServiceID returns the binding's service ID.
func (b DomainBinding) ServiceID() string { return b.service.id }

// Service returns the binding's service scope.
func (b DomainBinding) Service() Service { return b.service }

// EnvironmentID returns the binding's environment ID.
func (b DomainBinding) EnvironmentID() string { return b.service.environment.id }

// ProjectID returns the binding's project ID.
func (b DomainBinding) ProjectID() string { return b.service.environment.project.id }

// UserID returns the user ID the scope was minted for.
func (b DomainBinding) UserID() string { return b.service.environment.project.user.id }

// User returns the user identity the scope was minted for.
func (b DomainBinding) User() User { return b.service.environment.project.user }

// Role returns the membership role the scope was minted with.
func (b DomainBinding) Role() Role { return b.service.environment.project.role }

// Querier is the minimal read surface authorization needs. Both *sql.DB and
// *sql.Tx satisfy it.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Authorizer mints scopes against its querier. Construct one per database
// handle and share it; it holds no mutable state.
type Authorizer struct{ q Querier }

// NewAuthorizer returns an Authorizer reading membership from q. It panics on
// a nil querier so miswiring fails at construction, not on first use.
func NewAuthorizer(q Querier) *Authorizer {
	if q == nil {
		panic("authz: nil querier")
	}
	return &Authorizer{q: q}
}

// validNeed reports whether need is a valid access level.
func validNeed(need Access) bool { return need == Read || need == Write }

// check validates the Authorize inputs that must never reach SQL: an empty
// user ID (fail-closed must not depend on the schema forbidding ” rows) and
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

// AuthorizeProject proves user holds at least need access on a user project.
func (a *Authorizer) AuthorizeProject(ctx context.Context, user User, projectID string, need Access) (Project, error) {
	if err := check(user, need); err != nil {
		return Project{}, err
	}
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT m.role
		  FROM projects p
		  JOIN project_memberships m ON m.project_id = p.id
		 WHERE p.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		projectID, user.id, UserProjectKind).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, deny("project")
		}
		return Project{}, err
	}
	if !role.Allows(need) {
		return Project{}, deny("project")
	}
	return Project{user: user, id: projectID, role: role}, nil
}

// AuthorizeEnvironment proves user holds at least need access on the
// environment's project.
func (a *Authorizer) AuthorizeEnvironment(ctx context.Context, user User, environmentID string, need Access) (Environment, error) {
	if err := check(user, need); err != nil {
		return Environment{}, err
	}
	var projectID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT e.project_id, m.role
		  FROM environments e
		  JOIN projects p ON p.id = e.project_id
		  JOIN project_memberships m ON m.project_id = e.project_id
		 WHERE e.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		environmentID, user.id, UserProjectKind).Scan(&projectID, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Environment{}, deny("environment")
		}
		return Environment{}, err
	}
	if !role.Allows(need) {
		return Environment{}, deny("environment")
	}
	return Environment{project: Project{user: user, id: projectID, role: role}, id: environmentID}, nil
}

// AuthorizeService proves user holds at least need access on the service's
// project.
func (a *Authorizer) AuthorizeService(ctx context.Context, user User, serviceID string, need Access) (Service, error) {
	if err := check(user, need); err != nil {
		return Service{}, err
	}
	var environmentID, projectID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT s.environment_id, e.project_id, m.role
		  FROM services s
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN project_memberships m ON m.project_id = e.project_id
		 WHERE s.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		serviceID, user.id, UserProjectKind).Scan(&environmentID, &projectID, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Service{}, deny("service")
		}
		return Service{}, err
	}
	if !role.Allows(need) {
		return Service{}, deny("service")
	}
	project := Project{user: user, id: projectID, role: role}
	return Service{environment: Environment{project: project, id: environmentID}, id: serviceID}, nil
}

// AuthorizeVolume proves user holds at least need access on the volume's
// project.
func (a *Authorizer) AuthorizeVolume(ctx context.Context, user User, volumeID string, need Access) (Volume, error) {
	if err := check(user, need); err != nil {
		return Volume{}, err
	}
	var environmentID, projectID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT v.environment_id, e.project_id, m.role
		  FROM volumes v
		  JOIN environments e ON e.id = v.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN project_memberships m ON m.project_id = e.project_id
		 WHERE v.id = $1 AND m.user_id = $2 AND p.kind = $3`,
		volumeID, user.id, UserProjectKind).Scan(&environmentID, &projectID, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Volume{}, deny("volume")
		}
		return Volume{}, err
	}
	if !role.Allows(need) {
		return Volume{}, deny("volume")
	}
	project := Project{user: user, id: projectID, role: role}
	return Volume{environment: Environment{project: project, id: environmentID}, id: volumeID}, nil
}

// AuthorizeDomainBinding proves user holds at least need access on the
// binding's project.
func (a *Authorizer) AuthorizeDomainBinding(ctx context.Context, user User, hostname string, need Access) (DomainBinding, error) {
	if err := check(user, need); err != nil {
		return DomainBinding{}, err
	}
	// Normalize like the routing layer so "Example.COM." authorizes the same
	// binding it routes to; an invalid name simply matches no rows.
	hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
	var serviceID, environmentID, projectID string
	var role Role
	if err := a.q.QueryRowContext(ctx, `SELECT d.service_id, s.environment_id, e.project_id, m.role
		  FROM domain_bindings d
		  JOIN services s ON s.id = d.service_id
		  JOIN environments e ON e.id = s.environment_id
		  JOIN projects p ON p.id = e.project_id
		  JOIN project_memberships m ON m.project_id = e.project_id
		 WHERE d.hostname = $1 AND m.user_id = $2 AND p.kind = $3`,
		hostname, user.id, UserProjectKind).Scan(&serviceID, &environmentID, &projectID, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DomainBinding{}, deny("domain binding")
		}
		return DomainBinding{}, err
	}
	if !role.Allows(need) {
		return DomainBinding{}, deny("domain binding")
	}
	project := Project{user: user, id: projectID, role: role}
	service := Service{environment: Environment{project: project, id: environmentID}, id: serviceID}
	return DomainBinding{service: service, hostname: hostname}, nil
}
