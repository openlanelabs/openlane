<div align="center">

# OpenLane

**The open-source system of execution for post-sale delivery.**

*Track nothing. Execute everything.*

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Postgres](https://img.shields.io/badge/Postgres-16-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)
[![Next.js](https://img.shields.io/badge/Next.js-14-black?logo=next.js)](https://nextjs.org)

[Spec](docs/openlane_spec.md) · [Roadmap](#roadmap) · [Contributing](CONTRIBUTING.md) · [Discussions](https://github.com/orgs/openlanelabs/discussions)

</div>

---

Sales got Salesforce. Marketing got HubSpot. Post-sale delivery got spreadsheets, Slack threads, and hope.

OpenLane is a self-hostable, AI-native **PSA (Professional Services Automation)** platform that takes a customer from *"signed"* to *"live"* — projects, resources, time, money, and a customer portal your clients actually open.

## Why

Implementation managers juggle 15 customers over email and spend 60% of the week chasing updates. Commercial PSAs tax you per seat, per automation run, and lock the AI layer behind credits. We build the same thing — openly:

- **Self-hostable** — single Go binary + Postgres + Docker. Your data stays in your VPC.
- **Unlimited automations** — no per-run credit meters.
- **BYO-LLM** — OpenAI, Anthropic, or local Ollama. Agents never train on your data.
- **Customer portal that gets used** — magic links, no signups, 3-click tasks.
- **AGPL-3.0 core** — fork it, host it, just don't close-source our work.

## The pillars

| | Pillar | What it does |
|---|---|---|
| 1 | **Projects & Playbooks** | Versioned templates → projects in 30 seconds. Gantt, dependencies, baselines. |
| 2 | **Customer Portal** | Branded, magic-link, mobile-first. Customers complete tasks, upload files, approve milestones. |
| 3 | **Tasks & Views** | List, Kanban, Gantt, Calendar — same data, the view you want. |
| 4 | **Docs, Files, Forms, Sheets, Chat** | One "Spaces" surface — SOWs to migration mapping. |
| 5 | **Resources & Capacity** | Skills matrix, utilization heatmap, hiring forecast. |
| 6 | **Time** | Timer, calendar import, approvals. Invisible, not a chore. |
| 7 | **Money** | Budgets, rate cards, invoicing, margin truth with a "why" explainer. |
| 8 | **Integrations & API** | Salesforce, HubSpot, Jira, Slack. REST + webhooks + MCP. API-first. |
| 9 | **Open Nitro** | Agentic layer — Doc, Migration, Resourcing agents. Skills + guardians + **human approval gates**. |

Full detail: the [spec](docs/openlane_spec.md) is the build bible.

## Quick start (self-host)

```bash
git clone https://github.com/openlanelabs/openlane
cd openlane
docker compose up -d          # postgres + redis
cp .env.example .env          # if present; defaults work locally
make demo                     # migrate + seed + print a clickable portal URL
```

Requirements: Docker, Go 1.22+, Node 20+.

## Try the portal slice in 60 seconds

The first vertical slice (spec §25): a customer opens a magic link, sees
"Due for you", completes tasks one-tap — with Postgres RLS enforcing that a
customer can never see another tenant's data or internal-only tasks.

```bash
make db-up                    # postgres on :55432 (docker compose)
export DATABASE_URL="postgres://openlane:openlane@localhost:55432/openlane_test?sslmode=disable"
make demo                     # migrate + seed + print portal URL

# terminal 1 — API (static dev token enabled for the demo)
cd apps/api && OPENLANE_STAFF_TOKEN=dev-token OPENLANE_ALLOW_STATIC_TOKEN=1 go run ./cmd/api

# terminal 2 — web
cd apps/web && npm install && npm run dev
```

`make demo` prints a portal URL like `http://localhost:3000/p/<43-char-token>` —
open it. You should see **Adobe Onboarding** with **Due for you (3)**, one
task already **Overdue by 2d**, and none of the three internal tasks.
Tap **Complete** and watch the progress bar move; the row lands in
`audit_logs`.

To mint a fresh link (the way real staff would):

```bash
curl -s -X POST http://localhost:8080/v1/portal-links   -H "Authorization: Bearer dev-token"   -H "X-Workspace-Id: 11111111-1111-1111-1111-111111111111"   -H "Content-Type: application/json"   -d '{"project_id":"55555555-5555-5555-5555-555555555555","contact_id":"44444444-4444-4444-4444-444444444444"}'
```

The token prints exactly once (SHA-256 stored). Revoking the link makes the
portal show the friendly "This link has expired" page.

> The demo seed uses a deterministic token so the URL is printable — it is
> for demos only and must never run against a real database.

## Roadmap

- [ ] **P0 — MVP**: projects + templates + portal magic-link + tasks + docs/files/forms + time + notifications
- [ ] **P1 — PSA**: resources/capacity, budgets/invoicing, Jira two-way, portfolio dashboards, SSO
- [ ] **P2 — Open Nitro**: Doc/Migration/Signals agents, MCP server, approval gates + audit
- [ ] **P3 — Enterprise**: multi-currency, rev rec, EU residency, e-sign, marketplace

Track progress in [milestones](https://github.com/openlanelabs/openlane/milestones).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) — small diffs, API-first, RLS on every table, `make check` green. PRs welcome; link an issue first for anything non-trivial.

## Security

Found a vulnerability? **Do not open a public issue.** See [SECURITY.md](SECURITY.md) — security@princedotdev.is-a.bot, 48h acknowledgement.

## License

Copyright © 2026 OpenLane Labs.

- Core: **[AGPL-3.0](LICENSE)** — self-host freely, contribute changes back.
- Enterprise modules (SSO/SCIM, audit retention, data residency): commercial license.
