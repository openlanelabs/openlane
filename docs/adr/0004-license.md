# ADR-0004: AGPL-3.0 core, commercial enterprise modules

- Status: Accepted
- Date: 2025-09-09

## Context

We need a license that (a) lets companies self-host freely, (b) forces hosted forks to contribute changes back, (c) blocks cloud providers from rebranding our work as a closed service, and (d) leaves room for a sustainable business.

## Decision

- Core product: **AGPL-3.0**. Self-host, fork, modify — but network use triggers source obligations, so closed SaaS clones must open their changes.
- Enterprise modules (SSO/SAML, SCIM, extended audit retention, data residency, support SLAs): commercial license, separate distribution.

MIT was rejected explicitly: it would let a large vendor absorb our work closed-source (spec §24).

## Consequences

### Positive
- Trust moat for self-hosters; contributions flow back from serious forks
- Clear commercial surface that competitors can't clone from our public repo

### Negative
- Some enterprises ban AGPL outright — mitigated by offering the commercial license
- Contributions require AGPL CLA-style understanding; docs must be explicit

## Alternatives considered

| Alternative | Why not |
|---|---|
| MIT/Apache | No copyleft — closed fork is legal |
| BSL 1.1 | Converts to MIT later; friendlier to clones than we want |
| SSPL | OSI-rejected; hostile optics |
