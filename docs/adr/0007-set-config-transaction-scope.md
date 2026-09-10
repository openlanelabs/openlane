# ADR-0007: set_config transaction scope — is_local is only safe inside an explicit transaction

## Status

Accepted (2026-09-10)

## Context

OpenLane enforces tenancy through Postgres RLS driven by per-connection
settings (`app.workspace_id`, `app.user_id`, `app.portal_token_hash`).
Handlers scope every transaction with `set_config(...)`. pgx manages
connection pooling: an application connection is not one transaction —
it is a long-lived session that many requests borrow.

`set_config(..., is_local => true)` means "revert at the end of the
**current transaction**." Inside `pool.Begin()` that is exactly what we
want: the tx commits or rolls back, the setting vanishes, the pooled
connection comes back clean.

On an **autocommit statement** (no explicit `BEGIN`), each statement is
its own transaction. `is_local => true` reverts at the end of that one
statement. The setting is gone before the *next* statement runs.

## Decision

1. **Inside `pool.Begin()` txs**: `is_local => true` — always. Clean
   scoping per request.
2. **Outside an explicit tx** (pool-acquired conns, background paths,
   notify, health probes): `is_local => false` (session-level), and the
   code path must be a **self-contained acquire → set → query → release**
   block that re-sets every setting it relies on. Never assume an
   inherited session state on a pooled connection.
3. **Tests that assert RLS behavior** must run as the non-owner role
   (`openlane_app`); owners bypass RLS silently (ADR-0002 corollary).

## Consequences

Three production-shaped bugs so far all shared one root cause:
`is_local => true` on autocommit connections.

- Auth request/consume/refresh queried `memberships` with no workspace
  ctx — every login silently failed with no-oracle-shaped responses
  (PR #40).
- `notifyEvent` resolved `workspace_settings` after an is_local setting
  that had already reverted — `no rows` while the row existed (PR #46).

Both were invisible to the happy path because RLS failure modes look
like empty result sets, not errors. The debugging signature: **a row
that psql can see but the app cannot** — check the set_config scope
first.

## Alternatives considered

- `SET LOCAL` statement: same semantics, worse ergonomics (no
  expressions — ADR-0002 already rejected it).
- Application-name-derived policies: rejected — same trap, less
  explicit.
- pgx `pgxpool` BeforeAcquire hook setting a default: not possible to
  know the workspace before acquire; would just add a fourth way to
  get it wrong.
