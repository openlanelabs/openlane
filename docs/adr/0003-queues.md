# ADR-0003: River for queues in P0, Temporal in P2

- Status: Accepted
- Date: 2025-09-09

## Context

We need background jobs from day one (imports, webhooks, emails, magic-link sends) without new infrastructure for self-hosters. Agent workflows in P2 need durable, stateful orchestration.

## Decision

- **P0/P1:** River — Postgres-backed job queue. Transactional with our writes (outbox pattern for free), zero extra infra, web UI included.
- **P2:** Temporal Go SDK for agent workflows (multi-step, durable execution, approval-gated steps). River stays for fire-and-forget jobs.

Redis remains optional: rate-limit + cache only, droppable in P0.

## Consequences

### Positive
- Self-host: postgres + two binaries, no Kafka/RabbitMQ
- Jobs commit atomically with the data they serve — no dual-write bugs

### Negative
- Temporal brings a server we must ship in P2 — contained to agent workloads
- Job throughput capped by Postgres (fine: spec targets are modest)

## Alternatives considered

| Alternative | Why not |
|---|---|
| Redis queue (asynq) | Not transactional with Postgres writes |
| Kafka/NATS | Ops explosion for a self-hosted PSA |
| Cron only | No retries, no DLQ |
