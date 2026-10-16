# Frontend handoff: API tokens, MCP connect, OAuth

Paste the prompt below into a fresh agent session in this repository. It
is the complete frontend follow-up to the public API + MCP server work
(`docs/public-api.md`, `docs/mcp-server.md`). No backend changes are
needed for Phase 1; Phase 2 (OAuth) needs the small console-side
endpoints described there.

---

## Prompt

You are working in the ebpf-wg-mesh repository (a small PaaS: Go control
plane + TanStack Start console in `console/`). The backend for the
public API and MCP server is done; build the console frontend for it.
Read `docs/public-api.md`, `docs/mcp-server.md`, and `CONTEXT.md` first,
and follow existing console patterns exactly (feature folders under
`console/src/features/`, routes under `console/src/routes/`,
`createServerFn` server functions in
`console/src/lib/dashboard/server-functions.ts` and
`console/src/lib/dashboard/core/operations-*.server.ts`, the Connect
gateway in `console/src/lib/platform-grpc/`, StyleX + the UI kit in
`console/src/components/ui/`). Use Conventional Commits. Run
`bun --cwd=console run check`, `bun --cwd=console run typecheck`, and
`make test-unit-console` before calling anything done.

### Backend contract (already implemented, do not redesign)

PlatformService RPCs (regenerate with `bun --cwd=console run generate:proto`;
types land in `console/src/lib/platform-gen/platform_pb.ts`):

- `CreateAPIToken({name, scope, workspace_id, project_id, environment_id,
  access, expires_in_days}) -> {token, secret}`. Scopes:
  `API_TOKEN_SCOPE_ACCOUNT | WORKSPACE | PROJECT`; access:
  `API_TOKEN_ACCESS_READ | FULL` (unspecified = full). The `secret`
  (`wgm_...`) is returned **only** in this response.
- `ListAPITokens({workspace_id, include_revoked}) -> {tokens}`. Empty
  `workspace_id` lists the caller's own tokens; a workspace ID lists that
  workspace's tokens and requires workspace admin.
- `RevokeAPIToken({token_id})`. Creator or scoped-workspace admin.
- `APIToken` message fields: `id, name, prefix, scope, access,
  workspace_id, project_id, environment_id, created_by_user_id,
  created_at, expires_at, last_used_at, revoked_at`. `prefix` is the
  safe display fragment; never expect the secret again.

Rules enforced server-side (mirror them in form validation):
workspace tokens require a workspace admin creator; project tokens
require project write access and a matching project/environment pair;
name max 64 chars; expiry 0 (never) to 3650 days.

### Phase 1 — Token management UI + MCP connect page

1. **Gateway + operations.** Add `CreateAPIToken`, `ListAPITokens`,
   `RevokeAPIToken` to `PlatformGateway`
   (`console/src/lib/platform-grpc/gateway.server.ts`), operations in a
   new `console/src/lib/dashboard/core/operations-tokens.server.ts`
   (model on `operations-workspaces.server.ts` + its test), and server
   functions `loadApiTokens`, `doCreateApiToken`, `doRevokeApiToken` in
   `server-functions.ts`. The create operation returns the one-time
   secret to the client; it must never be logged or stored console-side.

2. **Tokens UI** (new feature folder `console/src/features/tokens/`):
   - A reusable token list (name, prefix, scope, access, created,
     expires, last used, revoked state) with revoke behind the existing
     confirm-dialog pattern (`console/src/components/delete-resource-dialog.tsx`).
   - A create dialog: name, scope selector (Account / workspace /
     project+environment depending on placement), access (Read-only /
     Full), expiry (Never / 30 / 90 / 365 days / custom). On success show
     the secret **once** with a copy button and the warning "You won't
     see this again"; also show ready-made MCP snippets (below).
   - Placements: personal account tokens get a "API tokens" section
     (add to the workspace shell navigation near People/Settings —
     account-scope tokens belong to the user, not a workspace, so they
     live above workspace context; if no user-level surface exists yet,
     add a Tokens tab to the workspace settings page that lists "Your
     tokens" via `ListAPITokens({})` plus, for workspace admins, the
     current workspace's tokens via `ListAPITokens({workspace_id})`).
     Project tokens get a "Tokens" section on Project Settings
     (`console/src/features/project-settings/`), always scoped to one
     chosen environment of that project.
   - Empty states explain token scopes in one line each (use the table
     in `docs/public-api.md`).

3. **MCP connect UI.** In the create-token success view and on a small
   "Connect an AI agent" card next to the token list, render per-editor
   tabs (Claude Code, Claude Desktop, Cursor, VS Code, Windsurf) with:
   - the one-command local setup using the `cmd/mcp` binary:
     `platform-mcp install --client <editor>` (see `docs/mcp-server.md`),
   - a copyable stdio JSON snippet (`command`, `PLATFORM_API_URL`,
     `PLATFORM_API_TOKEN`), and
   - when the deployment advertises a remote MCP URL (add
     `DASHBOARD_MCP_URL` env, may be empty), the remote JSON snippet
     (`url` + `Authorization: Bearer` header).
   The public API base URL comes from a new `DASHBOARD_PUBLIC_API_URL`
   env (may be empty → hide URL copy buttons, keep relative hints).
   Never place a real secret into a snippet except in the one-time
   success view.

4. **Tests.** Vitest unit tests modeled on
   `operations-workspaces.server.test.ts` (operations mapping + error
   surfacing), a component test for the create flow's one-time secret
   view (secret shown, copy wired, list refreshes, secret gone after
   close), and Playwright coverage in `console/tests/e2e/` if the local
   stack fixture makes it cheap (localteststack already serves the
   public API; `stack.json` has `public_api_url`).

Acceptance: a user can create each token scope from the right surface,
copy the secret exactly once, revoke tokens, and follow the connect
card to a working MCP session (`whoami` succeeds with the new token).

### Phase 2 — OAuth for the remote MCP server

Today `platform-mcp --http` accepts static API-token Bearer headers and
serves OAuth Protected Resource Metadata without an authorization
server. Make the console the authorization server so editors that
support MCP OAuth can connect interactively:

- Implement OAuth 2.1 authorization-code + PKCE with Dynamic Client
  Registration in the console server layer (new routes under
  `console/src/routes/auth/` siblings, e.g. `oauth/`): `authorize`
  (requires the existing console session; consent page showing the
  requesting client name, chosen workspace/project scope, and access),
  `token`, `register`, and metadata at
  `/.well-known/oauth-authorization-server`. Store clients, codes, and
  grants in the console Postgres store
  (`console/src/lib/dashboard/store/` — follow `migrations.server.ts` and
  `token-crypto.server.ts` patterns; hash client secrets and codes).
- The issued access token must be accepted by the MCP resource server
  and the public API. Simplest sound design: on token issuance, the
  console calls `CreateAPIToken` for the user with the consented scope
  and returns that secret as the OAuth access token (name it after the
  client, e.g. "MCP: Cursor"), with refresh rotation implemented as
  revoke-old + create-new. Revoking the API token (Phase 1 UI) then
  revokes the OAuth grant — show these tokens distinctly in the list.
- Point the MCP server's PRM `authorization_servers` at the console
  origin (`MCP_PUBLIC_URL`/new flag on the Go side — coordinate a small
  backend change to `internal/mcpserver/http.go`).
- Tests: registration, authorize→consent→code exchange with PKCE S256
  (bad verifier rejected), refresh rotation revokes the predecessor,
  and an end-to-end check that the issued token passes `GET /v1/token`.

Out of scope: changing token semantics in the control plane, new public
API endpoints, and any hosted API-documentation UI beyond linking
`GET /v1/openapi.json` from the tokens page.
