# Domain modules own platform behavior

Status: accepted

## Context

Platform behavior had accumulated in transport handlers, a console-wide service facade, and store methods shaped around historical RPC parameters. That made the protobuf schema, dashboard wiring, and database API competing descriptions of the same behavior.

## Decision

The control plane and console organize application behavior around the terms in [`CONTEXT.md`](../../CONTEXT.md). Protobuf is restricted to the network seam. Domain modules own validation, authorization, state transitions, and transactional effects. CockroachDB remains a concrete, locally substitutable dependency; it is not hidden behind table-shaped repository interfaces.

The first cutover establishes these rules:

- Environment release is the only command that applies service drafts.
- Restart, retry, rollback, cancel, removal, and exact redeploy are actions on persisted deployments.
- Persisted deployment lifecycle is the only source for deployment status; no inferred legacy lifecycle is retained.
- The console server is a composition root over focused operations, not a second behavior-owning service layer.

## Consequences

Transport and persistence changes may require direct call-site migrations because the project does not preserve compatibility yet. In return, each behavior has one owner, historical APIs can be deleted, and tests can exercise operations without reconstructing a broad facade.
