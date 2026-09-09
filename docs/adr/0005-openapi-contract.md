# ADR-0005: OpenAPI 3.1 contract as source of truth

- Status: Accepted
- Date: 2025-09-09

## Context

The spec demands API-first: every UI action hits the same public API, integrations and MCP build on it, and breaking changes need visibility. Hand-written handlers drift from docs and clients.

## Decision

`packages/contracts/openapi.yaml` is the single source of truth.

- Endpoints designed there first; `oapi-codegen` generates Go server types, `openapi-typescript` generates the web client
- CI lints the contract with Redocly; `main` stays releaseable
- Breaking changes require a new path version (`/v2`) discussion — surfaced in PR review via contract diff

## Consequences

### Positive
- UI, agents, MCP, and third-party integrations consume one contract — dogfooding is structural
- No "the docs are wrong" problem; docs are generated

### Negative
- Contract-first discipline has friction — a YAML edit before any handler lands
- Codegen config is one more thing to maintain

## Alternatives considered

| Alternative | Why not |
|---|---|
| Hand-written handlers + generated docs | Docs drift; integration teams suffer |
| gRPC | REST webhooks/portal token flows still need JSON; browser ergonomics worse |
| GraphQL | N+1 + permission footguns; overkill for a resource-oriented PSA |
