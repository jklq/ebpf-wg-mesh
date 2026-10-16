package bootstrap

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"ebof-wg-mesh/internal/controlplane"
	"ebof-wg-mesh/internal/platformdb"
)

// RunAPIToken is the host-admin offline path for public API tokens:
// mint the first token before any console UI exists, or rotate in an
// emergency. Day-to-day token management belongs to the dashboard RPCs.
//
// Usage: controlplane api-token create --user-id <id> --name <name>
// [--scope account|workspace|project] [--access read|full]
// [--workspace-id <id>] [--project-id <id> --environment-id <id>]
// [--expires-in-days N]
func RunAPIToken(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return fmt.Errorf("usage: controlplane api-token create [flags]")
	}
	fs := flag.NewFlagSet("controlplane api-token create", flag.ContinueOnError)
	var dbURL, userID, email, name, scope, access, workspaceID, projectID, environmentID string
	var expiresInDays int
	stringFlag(fs, &dbURL, "db-url", "CONTROLPLANE_DB_URL", "", "")
	stringFlag(fs, &userID, "user-id", "CONTROLPLANE_APITOKEN_USER_ID", "", "platform user ID the token acts as")
	stringFlag(fs, &email, "email", "CONTROLPLANE_APITOKEN_EMAIL", "", "verified email snapshot for invitation matching")
	stringFlag(fs, &name, "name", "CONTROLPLANE_APITOKEN_NAME", "", "token display name")
	stringFlag(fs, &scope, "scope", "CONTROLPLANE_APITOKEN_SCOPE", controlplane.APITokenScopeAccount, "account, workspace, or project")
	stringFlag(fs, &access, "access", "CONTROLPLANE_APITOKEN_ACCESS", controlplane.APITokenAccessFull, "read or full")
	stringFlag(fs, &workspaceID, "workspace-id", "CONTROLPLANE_APITOKEN_WORKSPACE_ID", "", "")
	stringFlag(fs, &projectID, "project-id", "CONTROLPLANE_APITOKEN_PROJECT_ID", "", "")
	stringFlag(fs, &environmentID, "environment-id", "CONTROLPLANE_APITOKEN_ENVIRONMENT_ID", "", "")
	fs.IntVar(&expiresInDays, "expires-in-days", 0, "0 means no expiry")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if dbURL == "" || userID == "" || name == "" {
		return fmt.Errorf("api-token create requires --db-url, --user-id, and --name")
	}
	if access != controlplane.APITokenAccessRead && access != controlplane.APITokenAccessFull {
		return fmt.Errorf("--access must be read or full")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := platformdb.OpenURL(ctx, dbURL, platformdb.SchemaPlatform)
	if err != nil {
		return err
	}
	defer db.Close()

	var member bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspace_memberships WHERE user_id = $1)`, userID).Scan(&member); err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if !member {
		return fmt.Errorf("user %q has no workspace membership; tokens act as their owner, sign in once first", userID)
	}
	in := controlplane.APITokenMint{Name: name, Scope: scope, Access: access, ExpiresInDays: int32(expiresInDays)}
	switch scope {
	case controlplane.APITokenScopeAccount:
	case controlplane.APITokenScopeWorkspace:
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = $1)`, workspaceID).Scan(&exists); err != nil {
			return fmt.Errorf("check workspace: %w", err)
		}
		if workspaceID == "" || !exists {
			return fmt.Errorf("--workspace-id must name an existing workspace")
		}
		in.WorkspaceID = workspaceID
	case controlplane.APITokenScopeProject:
		var resolvedProject, resolvedWorkspace string
		err := db.QueryRowContext(ctx,
			`SELECT p.id, COALESCE(p.workspace_id, '') FROM environments e
			 JOIN projects p ON p.id = e.project_id
			 WHERE e.id = $1 AND p.kind = 'user'`, environmentID).Scan(&resolvedProject, &resolvedWorkspace)
		if err != nil {
			return fmt.Errorf("resolve environment: %w (environment must exist)", err)
		}
		if projectID != "" && projectID != resolvedProject {
			return fmt.Errorf("--project-id does not own --environment-id")
		}
		if resolvedWorkspace == "" {
			return fmt.Errorf("project has no workspace (managed project)")
		}
		in.WorkspaceID, in.ProjectID, in.EnvironmentID = resolvedWorkspace, resolvedProject, environmentID
	default:
		return fmt.Errorf("--scope must be account, workspace, or project")
	}
	rec, secret, err := controlplane.CreateAPITokenOffline(ctx, db, userID, email, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created api token %s (%s, scope=%s access=%s) for user %s; the secret below is shown once\n",
		rec.ID, rec.Prefix, rec.Scope, rec.Access, userID)
	fmt.Println(secret)
	return nil
}
