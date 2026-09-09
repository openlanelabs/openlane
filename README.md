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
docker compose up -d
# → http://localhost:3000
```

Requirements: Docker. That's it.

## Roadmap

- [ ] **P0 — MVP**: projects + templates + portal magic-link + tasks + docs/files/forms + time + notifications
- [ ] **P1 — PSA**: resources/capacity, budgets/invoicing, Jira two-way, portfolio dashboards, SSO
- [ ] **P2 — Open Nitro**: Doc/Migration/Signals agents, MCP server, approval gates + audit
- [ ] **P3 — Enterprise**: multi-currency, rev rec, EU residency, e-sign, marketplace

Track progress in [milestones](https://github.com/openlanelabs/openlane/milestones).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) — small diffs, API-first, RLS on every table, `make check` green. PRs welcome; link an issue first for anything non-trivial.

## Security

Found a vulnerability? **Do not open a public issue.** See [SECURITY.md](SECURITY.md) — security@openlane.dev, 48h acknowledgement.

## License

Copyright © 2025 OpenLane Labs.

- Core: **[AGPL-3.0](LICENSE)** — self-host freely, contribute changes back.
- Enterprise modules (SSO/SCIM, audit retention, data residency): commercial license.
