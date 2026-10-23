# Public API

The public API is the supported way to integrate with the platform from
CI, scripts, and AI agents. It is a versioned REST/JSON API (`/v1`) served
by the control plane and authenticated with API tokens. The MCP server
([mcp-server.md](mcp-server.md)) is built on it. The console keeps using
the internal mTLS API; nothing about that path changes.

## Connecting

Set `CONTROLPLANE_PUBLIC_API_LISTEN` (default `127.0.0.1:9445`). The
listener serves plaintext HTTP: expose it publicly only behind TLS
termination (ingress or a reverse proxy), never as bare HTTP. Empty
disables it.

```bash
curl https://api.example.test/v1/me \
  -H "Authorization: Bearer wgm_..."
```

Every request runs as the token's owning user, so workspace membership
and roles apply exactly as in the console.

## API tokens

| Token scope | Reaches | Created by | Best for |
|---|---|---|---|
| Account | Everything the user can | The user | Personal scripts, local agents |
| Workspace | One workspace | Workspace admin | Team CI/CD, shared automation |
| Project | One environment of one project | Project writer | Deploy pipelines, least privilege |

Each token also has an access level: `full` (read/write) or `read`
(reads only). Tokens may expire (`expires_in_days`); revoked and expired
tokens fail closed. Only the SHA-256 hash of the secret is stored; the
secret (`wgm_...`, recognizable by secret scanners) is shown once at
creation.

Create and manage tokens with the dashboard-authenticated RPCs
`CreateAPIToken` / `ListAPITokens` / `RevokeAPIToken` (console UI is
deferred work, see the handoff notes), or offline as a host admin:

```bash
controlplane api-token create --db-url "$CONTROLPLANE_DB_URL" \
  --user-id <user-id> --name "ci" --scope project \
  --environment-id <env-id> --access full --expires-in-days 90
```

`make dev-ephemeral` (localteststack) starts the API on loopback and
mints a dev account token, printed in the startup log and recorded in
`console/artifacts/e2e-local/stack.json` (`public_api_url`, `dev_api_token`).

Scoped-token rules beyond the owner's memberships:

- Workspace tokens are confined to their workspace; project tokens to
  their environment (plus read access to their project and its usage
  filtered to that environment).
- Identity and membership management (`/v1/me`, invitations, member
  roles, workspace rename/delete, billing mutations, project move)
  requires account tokens. Project/environment lifecycle requires at
  least workspace tokens.
- Read tokens may only call read operations.

## Shape of the API

Bodies are the protobuf JSON mapping of the `platform.v1` messages with
proto field names (snake_case; 64-bit integers as strings). The route
table and schemas are the contract:

- `GET /v1/openapi.json` — generated OpenAPI 3.1 document (also how you
  discover every endpoint, parameter, and body schema).
- Resources: workspaces and members, projects, environments and
  releases, services (drafts, scale, discard), deployments and actions,
  logs, build artifacts and attempts, domain bindings, volumes, source
  inspection and GitHub linking, project usage, billing, fleet agents
  (operators), and `GET /v1/me` / `GET /v1/token` introspection.

Vocabulary follows [CONTEXT.md](../CONTEXT.md). Two semantics matter:

- **Staged drafts.** Service creates/updates and volume creates stage a
  service draft. Nothing runs until `POST /v1/environments/{id}/releases`.
  `Service.pending_changes` and `unapplied_changes` show what waits.
- **Tombstone deletes.** Deletes are restorable during the grace period
  (`POST .../restore`); name confirmations guard workspace, project,
  production-environment, and volume deletion.
  `GET .../deletion-preview` shows what a delete would tombstone.
- **Serverless services.** `spec.serverless_enabled` makes a service
  scale to zero when idle; it is staged like every spec field.
  `Service.serverless_state` reports `SERVERLESS_STATE_AWAKE`,
  `SERVERLESS_STATE_ASLEEP`, or `SERVERLESS_STATE_WAKING`. See
  [Serverless services (scale-to-zero)](serverless.md).

## Errors, request IDs, retries

Every error has one shape, and every response carries `X-Request-Id`
(accepted from the client or generated; quote it in support requests):

```json
{"error": {"code": "not_found", "message": "...", "request_id": "..."}}
```

Codes: `invalid_argument` (400), `unauthenticated` (401),
`permission_denied` (403), `not_found` (404), `conflict` /
`failed_precondition` (409), `rate_limited` (429), `unavailable` (503),
`deadline_exceeded` (504), `internal` (500, detail withheld). Denied
access to another workspace's resource is indistinguishable from a
missing resource above the token-scope layer.

Reads are safe to retry. Writes: retry only when no response was
received; deployment actions accept `idempotency_key` for exactly that.

## Rate limits

Per token, per control-plane replica: `CONTROLPLANE_PUBLIC_API_RPH`
requests/hour (default 10000) and `CONTROLPLANE_PUBLIC_API_RPS` burst
(default 50/s). Responses carry `RateLimit-Policy`, `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset`; 429s add `Retry-After`.
With N replicas a token can use up to N times the quota — limits are
protective, not billing meters.

## Versioning

`/v1` is the contract. Additive changes (new endpoints, new optional
fields) land in v1; breaking changes get a new version prefix. Field
shapes follow the platform.v1 protobuf messages, which evolve under
protobuf compatibility rules.

## Operational notes

- The listener runs on every control-plane replica; calls execute
  in-process as the delegated user (no extra hop, no shared credential).
- During recovery pause, mutations return 503 and reads keep serving,
  matching the internal transports.
- Token audit: `api_tokens.created_by_user_id`, `created_at`,
  `last_used_at` (updated at most once a minute), `revoked_at`.
