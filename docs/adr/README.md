# Architecture Decision Records

Major decisions are recorded here, one file per decision. New ADRs copy `0000-template.md` and take the next number. They are numbered sequentially and never edited once accepted — supersede instead.

| # | Decision | Status |
|---|---|---|
| 0001 | Go monolith with Chi + pgx + sqlc, not microservices | Accepted |
| 0002 | Postgres RLS as the single tenancy enforcement layer | Accepted |
| 0003 | River (Postgres-backed) for queues in P0; Temporal in P2 | Accepted |
| 0004 | AGPL-3.0 for core; commercial license for enterprise modules | Accepted |
| 0005 | OpenAPI 3.1 in `packages/contracts` as source of truth | Accepted |
| 0006 | Agent attestations + mechanical approval gates (Open Nitro) | Accepted (P2) |
- [0007 — set_config transaction scope (is_local trap)](0007-set-config-transaction-scope.md)
