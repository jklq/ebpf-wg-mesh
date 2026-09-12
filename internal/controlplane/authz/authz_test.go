package authz

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAuthenticatedUser(t *testing.T) {
	t.Parallel()

	user, err := AuthenticatedUser("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID() != "user-1" {
		t.Fatalf("user id = %q", user.ID())
	}
	if _, err := AuthenticatedUser(""); err == nil {
		t.Fatal("empty user id accepted")
	}
	if _, err := AuthenticatedUser("   "); err == nil {
		t.Fatal("blank user id accepted")
	}
}

func TestRoleAllows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		role  Role
		need  Access
		allow bool
	}{
		{RoleOwner, Read, true},
		{RoleOwner, Write, true},
		{RoleEditor, Read, true},
		{RoleEditor, Write, true},
		{RoleViewer, Read, true},
		{RoleViewer, Write, false},
		{Role(""), Read, false},
		{Role(""), Write, false},
		{Role("admin"), Read, false},
		{Role("admin"), Write, false},
		{RoleOwner, Access(0), false},
		{RoleOwner, Access(99), false},
	}
	for _, tc := range cases {
		if got := tc.role.Allows(tc.need); got != tc.allow {
			t.Fatalf("role %q allows %d = %v, want %v", tc.role, tc.need, got, tc.allow)
		}
	}
}

func TestZeroScopesAuthorizeNothing(t *testing.T) {
	t.Parallel()

	// Scopes carry unexported state, so the only way to obtain IDs without
	// the Authorizer is the zero value. Zero scopes must match no rows:
	// every accessor below feeds a SQL predicate, and "" matches nothing.
	var user User
	if user.ID() != "" {
		t.Fatalf("zero user id = %q", user.ID())
	}
	var project Project
	if project.ID() != "" || project.UserID() != "" {
		t.Fatalf("zero project leaks ids: %+v", project)
	}
	var env Environment
	if env.ID() != "" || env.ProjectID() != "" || env.UserID() != "" {
		t.Fatalf("zero environment leaks ids: %+v", env)
	}
	var service Service
	if service.ID() != "" || service.EnvironmentID() != "" || service.ProjectID() != "" || service.UserID() != "" {
		t.Fatalf("zero service leaks ids: %+v", service)
	}
	var volume Volume
	if volume.ID() != "" || volume.EnvironmentID() != "" || volume.ProjectID() != "" || volume.UserID() != "" {
		t.Fatalf("zero volume leaks ids: %+v", volume)
	}
	var binding DomainBinding
	if binding.Hostname() != "" || binding.ServiceID() != "" || binding.ProjectID() != "" || binding.UserID() != "" {
		t.Fatalf("zero binding leaks ids: %+v", binding)
	}
	var operator Operator
	if operator.UserID() != "" {
		t.Fatalf("zero operator leaks id: %+v", operator)
	}
}

// stubConnector is a scripted database/sql driver so Authorize* unit tests run
// without a database. Each case gets its own *sql.DB via sql.OpenDB.
type stubConnector struct {
	columns  []string
	rows     [][]driver.Value
	queryErr error
	lastArgs *[]driver.NamedValue
}

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("authz stub: use connector")
}

func (c stubConnector) Connect(context.Context) (driver.Conn, error) { return &stubConn{c: c}, nil }

func (stubConnector) Driver() driver.Driver { return stubDriver{} }

type stubConn struct{ c stubConnector }

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("authz stub: no statements")
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) Begin() (driver.Tx, error) { return nil, errors.New("authz stub: no transactions") }

func (c *stubConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	if c.c.lastArgs != nil {
		*c.c.lastArgs = append([]driver.NamedValue(nil), args...)
	}
	if c.c.queryErr != nil {
		return nil, c.c.queryErr
	}
	return &stubRows{columns: c.c.columns, rows: c.c.rows}, nil
}

type stubRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *stubRows) Columns() []string { return r.columns }

func (r *stubRows) Close() error { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

func openStubDB(columns []string, rows [][]driver.Value, queryErr error, lastArgs *[]driver.NamedValue) *sql.DB {
	return sql.OpenDB(stubConnector{columns: columns, rows: rows, queryErr: queryErr, lastArgs: lastArgs})
}

func TestAuthorizerTable(t *testing.T) {
	t.Parallel()

	user, err := AuthenticatedUser("user-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	boom := errors.New("connection refused")

	type outcome struct {
		ids []string
		err error
	}
	cases := []struct {
		name       string
		columns    []string
		rows       [][]driver.Value
		queryErr   error
		call       func(a *Authorizer) outcome
		wantIDs    []string
		wantDenied bool
		wantNoRows bool
		wantPlain  bool
	}{
		{
			name:    "project owner write",
			columns: []string{"role"}, rows: [][]driver.Value{{"owner"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeProject(ctx, user, "project-1", Write)
				return outcome{[]string{scope.ID(), scope.UserID(), string(scope.Role())}, err}
			},
			wantIDs: []string{"project-1", "user-1", "owner"},
		},
		{
			name:    "project viewer read",
			columns: []string{"role"}, rows: [][]driver.Value{{"viewer"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeProject(ctx, user, "project-1", Read)
				return outcome{[]string{scope.ID()}, err}
			},
			wantIDs: []string{"project-1"},
		},
		{
			name:    "project viewer write denied",
			columns: []string{"role"}, rows: [][]driver.Value{{"viewer"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "project-1", Write)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "project unknown role denied",
			columns: []string{"role"}, rows: [][]driver.Value{{"admin"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "project-1", Read)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "project missing denied",
			columns: []string{"role"},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "missing", Read)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "project db error passes through",
			columns: []string{"role"}, queryErr: boom,
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "project-1", Read)
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "project invalid need rejected",
			columns: []string{"role"}, rows: [][]driver.Value{{"owner"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "project-1", Access(99))
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "project zero need rejected",
			columns: []string{"role"}, rows: [][]driver.Value{{"owner"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, user, "project-1", Access(0))
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "project empty user rejected",
			columns: []string{"role"}, rows: [][]driver.Value{{"owner"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeProject(ctx, User{}, "project-1", Read)
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "environment allow mints ids",
			columns: []string{"project_id", "role"}, rows: [][]driver.Value{{"project-1", "editor"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeEnvironment(ctx, user, "env-1", Write)
				return outcome{[]string{scope.ID(), scope.ProjectID(), scope.UserID()}, err}
			},
			wantIDs: []string{"env-1", "project-1", "user-1"},
		},
		{
			name:    "environment viewer write denied",
			columns: []string{"project_id", "role"}, rows: [][]driver.Value{{"project-1", "viewer"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeEnvironment(ctx, user, "env-1", Write)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "environment invalid need rejected",
			columns: []string{"project_id", "role"}, rows: [][]driver.Value{{"project-1", "owner"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeEnvironment(ctx, user, "env-1", Access(0))
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "service allow mints ids",
			columns: []string{"environment_id", "project_id", "role"}, rows: [][]driver.Value{{"env-1", "project-1", "owner"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeService(ctx, user, "service-1", Write)
				return outcome{[]string{scope.ID(), scope.EnvironmentID(), scope.ProjectID()}, err}
			},
			wantIDs: []string{"service-1", "env-1", "project-1"},
		},
		{
			name:    "service viewer write denied",
			columns: []string{"environment_id", "project_id", "role"}, rows: [][]driver.Value{{"env-1", "project-1", "viewer"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeService(ctx, user, "service-1", Write)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "service empty user rejected",
			columns: []string{"environment_id", "project_id", "role"}, rows: [][]driver.Value{{"env-1", "project-1", "owner"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeService(ctx, User{}, "service-1", Read)
				return outcome{nil, err}
			},
			wantPlain: true,
		},
		{
			name:    "volume allow mints ids",
			columns: []string{"environment_id", "project_id", "role"}, rows: [][]driver.Value{{"env-1", "project-1", "editor"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeVolume(ctx, user, "volume-1", Read)
				return outcome{[]string{scope.ID(), scope.EnvironmentID(), scope.ProjectID()}, err}
			},
			wantIDs: []string{"volume-1", "env-1", "project-1"},
		},
		{
			name:    "volume missing denied",
			columns: []string{"environment_id", "project_id", "role"},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeVolume(ctx, user, "missing", Read)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "binding allow mints ids",
			columns: []string{"service_id", "environment_id", "project_id", "role"}, rows: [][]driver.Value{{"service-1", "env-1", "project-1", "owner"}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeDomainBinding(ctx, user, "web.example.com", Read)
				return outcome{[]string{scope.Hostname(), scope.ServiceID(), scope.EnvironmentID(), scope.ProjectID()}, err}
			},
			wantIDs: []string{"web.example.com", "service-1", "env-1", "project-1"},
		},
		{
			name:    "binding viewer write denied",
			columns: []string{"service_id", "environment_id", "project_id", "role"}, rows: [][]driver.Value{{"service-1", "env-1", "project-1", "viewer"}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeDomainBinding(ctx, user, "web.example.com", Write)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "operator allow",
			columns: []string{"one"}, rows: [][]driver.Value{{int64(1)}},
			call: func(a *Authorizer) outcome {
				scope, err := a.AuthorizeOperator(ctx, user)
				return outcome{[]string{scope.UserID()}, err}
			},
			wantIDs: []string{"user-1"},
		},
		{
			name:    "operator missing denied",
			columns: []string{"one"},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeOperator(ctx, user)
				return outcome{nil, err}
			},
			wantDenied: true, wantNoRows: true,
		},
		{
			name:    "operator empty user rejected",
			columns: []string{"one"}, rows: [][]driver.Value{{int64(1)}},
			call: func(a *Authorizer) outcome {
				_, err := a.AuthorizeOperator(ctx, User{})
				return outcome{nil, err}
			},
			wantPlain: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openStubDB(tc.columns, tc.rows, tc.queryErr, nil)
			t.Cleanup(func() { _ = db.Close() })
			got := tc.call(NewAuthorizer(db))
			switch {
			case tc.wantPlain:
				if got.err == nil || errors.Is(got.err, ErrDenied) || errors.Is(got.err, sql.ErrNoRows) {
					t.Fatalf("err = %v, want plain non-denial error", got.err)
				}
			case tc.wantDenied || tc.wantNoRows:
				if !errors.Is(got.err, ErrDenied) || !errors.Is(got.err, sql.ErrNoRows) {
					t.Fatalf("err = %v, want ErrDenied wrapping ErrNoRows", got.err)
				}
			default:
				if got.err != nil {
					t.Fatalf("err = %v", got.err)
				}
				if strings.Join(got.ids, "\x00") != strings.Join(tc.wantIDs, "\x00") {
					t.Fatalf("ids = %q, want %q", got.ids, tc.wantIDs)
				}
			}
		})
	}
}

func TestAuthorizerNormalizesBindingHostname(t *testing.T) {
	t.Parallel()

	user, err := AuthenticatedUser("user-1")
	if err != nil {
		t.Fatal(err)
	}
	var args []driver.NamedValue
	db := openStubDB(
		[]string{"service_id", "environment_id", "project_id", "role"},
		[][]driver.Value{{"service-1", "env-1", "project-1", "owner"}},
		nil, &args,
	)
	t.Cleanup(func() { _ = db.Close() })
	scope, err := NewAuthorizer(db).AuthorizeDomainBinding(context.Background(), user, "  Web.Example.COM. ", Read)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Hostname() != "web.example.com" {
		t.Fatalf("minted hostname = %q", scope.Hostname())
	}
	if len(args) != 3 {
		t.Fatalf("query args = %v, want 3", args)
	}
	if args[0].Value != "web.example.com" || args[1].Value != "user-1" || args[2].Value != UserProjectKind {
		t.Fatalf("query args = %v", args)
	}
}

func TestNewAuthorizerRejectsNil(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("NewAuthorizer(nil) did not panic")
		}
	}()
	_ = NewAuthorizer(nil)
}
