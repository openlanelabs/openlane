# ADR-0001: Go modular monolith with Chi, pgx, sqlc

- Status: Accepted
- Date: 2026-09-09

## Context

OpenLane's spec targets a small team shipping a 9-pillar PSA. Microservices would spend our complexity budget on infrastructure before the product earns it. We need fast iteration, single-binary deploys for self-hosters, and type-safe SQL with Postgres RLS.

## Decision

One Go module, modular by package boundary:

- `apps/api` — Chi router, stdlib-compatible, handlers per resource
- `apps/worker` — same module, River jobs + agent runners
- `apps/web` — Next.js 14, no business logic, talks only to the public API
- `db/` — goose migrations + sqlc queries; generated code checked in
- `packages/contracts/` — OpenAPI 3.1 YAML is the single source of truth; generates both the TS client and Go server types

Split into services only when a worker's resource profile demands it (expected: agent runner in P2).

## Consequences

### Positive
- One deploy unit for self-hosters; `docker compose up` runs the whole product
- sqlc gives compile-time SQL correctness; pgx v5 is the fastest Postgres driver
- No network hops for 99% of requests in P0/P1

### Negative
- Repo discipline required — package boundaries are conventions, not hard walls
- One giant test suite unless we partition (mitigated: per-app matrix in CI)

### Neutral
- Team must know Go, not Node, on the backend

## Alternatives considered

| Alternative | Why not |
|---|---|
| Node/NestJS monolith | Weaker RLS/pgx ergonomics, runtime perf ceiling |
| Microservices day 1 | Ops tax before product-market; spec says monolith first |
| Rails/Django | Conflicts with the single-binary self-host story |
