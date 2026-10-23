# MCP server

The MCP server connects AI coding agents to the platform through the
[public API](public-api.md) — it never talks to the control plane
directly. It creates projects and services, prepares drafts, releases
environments, manages domains, volumes, and variables, and debugs
deployments. `cmd/mcp` is the binary; `internal/mcpserver` is the tool
implementation.

## Connect an editor

You need an API token (see [public-api.md](public-api.md#api-tokens)).

```bash
go build -o bin/platform-mcp ./cmd/mcp
export PLATFORM_API_URL=https://api.example.test
export PLATFORM_API_TOKEN=wgm_...
bin/platform-mcp install --client claude-code   # or cursor, vscode, windsurf, claude-desktop
```

`install` merges the entry into the editor's existing config without
disturbing other servers. `platform-mcp config --client X` prints the
snippet instead. Manual stdio config:

```json
{
  "mcpServers": {
    "ebpf-wg-mesh": {
      "command": "/path/to/platform-mcp",
      "args": [],
      "env": {
        "PLATFORM_API_URL": "https://api.example.test",
        "PLATFORM_API_TOKEN": "wgm_..."
      }
    }
  }
}
```

## Remote (streamable HTTP)

```bash
PLATFORM_API_URL=http://127.0.0.1:9445 \
MCP_PUBLIC_URL=https://mcp.example.test/mcp \
bin/platform-mcp --http --http-listen 127.0.0.1:8080
```

Clients send their own API token as `Authorization: Bearer wgm_...`;
the server validates it against the public API and binds each session
to that token (stateless, any replica works). Unauthenticated requests
get `401` with `WWW-Authenticate` pointing at
`/.well-known/oauth-protected-resource/mcp`. Full OAuth (consent +
dynamic client registration in the console) is deferred work; today
remote clients must support static Bearer headers.

## Example prompts

- "Create a Next.js app here and deploy it; also assign it a domain."
- "Deploy a Postgres database" (`create_service` with image
  `postgres:16`, a volume, then release).
- "Pull the variables of my api service into a .env file."
- "Figure out why my backend is crashing" (`diagnose_service`).
- "Redeploy my api service in production."
- "What did staging cost this month?" (`get_project_usage`, billing).

Linked defaults mirror `railway link`: set `PLATFORM_WORKSPACE_ID`,
`PLATFORM_PROJECT_ID`, `PLATFORM_ENVIRONMENT_ID`, `PLATFORM_SERVICE_ID`
and tools use them when arguments are omitted.

## Tools

Account: `whoami`. Workspaces: `list_workspaces`, `get_workspace`,
`create_workspace`, `rename_workspace`, `delete_workspace`,
`list_workspace_members`, `invite_workspace_member`,
`revoke_workspace_invitation`, `accept_workspace_invitation`,
`decline_workspace_invitation`, `update_workspace_member_role`,
`remove_workspace_member`.

Projects and environments: `list_projects`, `create_project`,
`get_project`, `delete_project`, `restore_project`, `move_project`,
`update_project_log_retention`, `list_environments`,
`create_environment`, `get_environment`, `environment_status`,
`rename_environment`, `duplicate_environment`,
`set_environment_auto_deploy`, `delete_environment`,
`restore_environment`, `release_environment`, `deploy` (alias).

Services and variables: `list_services`, `get_service`,
`get_service_status`, `create_service`, `update_service`,
`remove_service`, `restore_service`, `scale_service`,
`discard_service_changes`, `list_variables`, `set_variables`.
`create_service` and `update_service` accept `serverless_enabled` to make
a service scale to zero when idle (see
[Serverless services (scale-to-zero)](serverless.md)); service reads
report `serverless_state`.

Deployments and builds: `list_deployments`, `redeploy`,
`restart_service`, `rollback_deployment`, `deployment_action`,
`list_build_attempts`, `list_service_artifacts`.

Domains and volumes: `list_domains`, `domain_status`,
`generate_domain`, `create_domain`, `update_domain`, `delete_domain`,
`restore_domain`, `list_volumes`, `get_volume`, `create_volume`,
`update_volume`, `delete_volume`.

Observability and cost: `get_logs`, `get_build_logs`, `http_logs`,
`service_metrics`, `get_project_usage`, `diagnose_service`,
`list_fleet_agents` (operators). Billing: `get_billing_account`,
`list_billing_statements`, `get_billing_statement`,
`update_billing_budget`, `change_billing_plan`,
`start_billing_payment_setup`, `cancel_billing_account`. Source:
`inspect_source`, `link_github_repository`. Docs: `docs_search`,
`docs_fetch`, plus every `docs/*.md` as a `platform://docs/...`
resource and the `deploy_app` / `debug_deployment` prompts.

## How semantics map from Railway

- **Staged changes**: Railway's `accept-deploy` is `release_environment`.
  Service tools say whether work is staged and accept `release: true`.
- **Variables**: values live in the service draft runtime env; there is
  no `${{ }}` reference syntax — read the source value and set it.
- **No templates, feature flags, TCP proxies, or buckets**: the
  platform has no such features. Databases are ordinary services from
  direct images with a volume mount.
- **`railway-agent`**: there is no server-side AI agent; `diagnose_service`
  returns the same evidence bundle (status, deployments, build, logs,
  domains) with `likely_issues` for the calling model to reason over.
- **HTTP metrics**: request/error/latency aggregates do not exist as an
  API; `http_logs` exposes ingress request logs and `service_metrics`
  exposes resource usage instead.

## Security considerations

- Stdio mode reads the token from the environment, not the editor
  config file when you interpolate variables; prefer read-only or
  project-scoped tokens for agents.
- Destructive tools carry protocol-level `destructiveHint` and require
  a preview call followed by `confirm: true`; clients should still
  prompt. Deletion name confirmations are resolved server-side after
  confirmation.
- `list_variables` returns variable names only by default. Passing
  `include_values: true` reveals secret values and requires a
  full-access token whose user holds secret-read access (workspace
  member or admin); treat transcripts accordingly. Service reads
  (`get_service`, `list_services`) always redact values.
- The token acts as its owner for billing and audit (`last_used_at`
  per token); revoke lost tokens immediately.
