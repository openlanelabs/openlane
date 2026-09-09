# ADR-0002: Postgres RLS as the tenancy enforcement layer

- Status: Accepted
- Date: 2025-09-09

## Context

The single worst failure mode for a multi-tenant PSA is one customer reading another's data (spec: Security Bible, EC-PORTAL IDOR cases). Application-layer checks are forgettable; one missed WHERE clause is a breach.

## Decision

Every table carries `workspace_id` (and `created_by`). Postgres Row-Level Security policies enforce tenant + visibility. The Go API sets `app.workspace_id` / `app.user_id` via `SET LOCAL` per transaction (pgx transaction-level), so a query cannot forget its tenant.

CI enforces:
1. Migrations must apply cleanly
2. A DB-level check fails if any table lacks `workspace_id` or an RLS policy (see `.github/workflows/ci.yml` → `db` job)

Portal tokens are scoped further: resolved to `(workspace_id, project_id, email)` — the RLS session for portal requests also filters by project.

## Consequences

### Positive
- The database refuses to leak, even with a buggy query
- IDOR testing becomes a query-level property, easy to fuzz

### Negative
- Every connection/tx must set the GUCs — one missing `SET LOCAL` means wrong-scope or empty results (fail-safe: policies default-deny)
- RLS adds per-query overhead; acceptable at our scale targets (p95 <300ms)

### Neutral
- Analytics uses a separate role that bypasses RLS with read-only, row-filtered views

## Alternatives considered

| Alternative | Why not |
|---|---|
| App-layer checks only | Forgetting is a matter of time; breaches unacceptable |
| Separate DB per tenant | Operational explosion for 10k workspaces |
| Postgres schemas per tenant | Same explosion; migrations get painful |
