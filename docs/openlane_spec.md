# OPENLANE — The Open-Source Agentic PSA Bible
> Version 1.0 — Master Build Reference
> Goal: Beat Rocketlane ($60M Series C, ~$105M total, 750+ customers) with a self-hostable, open-source, AI-native Professional Services Automation platform.
> Audience: Even a Day-1 fresher can read top-to-bottom and build a 100/10 SaaS from this doc.
> How to use: Build phase-by-phase (P0 -> P3). Every feature has: WHAT + WHY + WHO + HAPPY PATH + EDGE CASES + RULES + API + DB + TESTS.

---

## 0. TABLE OF CONTENTS

1. Name Options + Recommendation
2. One-Paragraph Vision + Why We Win
3. ELI5 + Glossary (newbie start here)
4. Who Uses This? Personas + Real Jobs
5. Product Map — 9 Pillars
6. Pillar 1: Projects & Templates
7. Pillar 2: Customer Portal (magic-link)
8. Pillar 3: Tasks, Dependencies, Views
9. Pillar 4: Docs, Files, Forms, Sheets, Chat
10. Pillar 5: Resources & Capacity
11. Pillar 6: Time Tracking
12. Pillar 7: Money — Budgets, Rate Cards, Billing, Invoicing, Revenue
13. Pillar 8: Integrations & API & Webhooks & MCP
14. Pillar 9: Analytics, Signals, CSAT, Portfolio
15. OPEN NITRO — Agentic Layer (our Nitro killer)
16. Cross-Cutting: Auth, Permissions, Notifications, Automations, Search
17. Security Bible
18. Scalability + Reliability Bible
19. Tech Architecture + Repo Layout + DB Schema Core
20. UI/UX Design System
21. 40 Better Features (beyond Rocketlane)
22. Edge-Case Encyclopedia (200+ cases)
23. Build Roadmap P0-P3 + Acceptance Criteria
24. Open-Source + Pricing Strategy
25. What To Build First Tomorrow Morning

---

## 1. NAME OPTIONS

Shortlist (check domain + trademark before final):

1. **OpenLane** — clearest. "Open-source Rocketlane." SEO win. Recommended.
2. **GoLive OS** — outcome language. "We get you live." Great for marketing.
3. **AfterSale** — owns post-sale category. Memorable.
4. **LaunchLane** — friendly, onboarding energy.
5. **Deliverly** — delivery + simply. SaaSy.
6. **Portl** — portal-first.
7. **OnboardOS / Shiproom / Handoff**

**Recommendation: `OpenLane` for code + GitHub (openlane-dev/openlane), marketed as `OpenLane — GoLive OS`.** Tagline: "The open-source system of execution for post-sale delivery. Track nothing. Execute everything."

Runner-up if you want distinct brand: `AfterSale`.

---

## 2. VISION IN ONE PARAGRAPH

Sales got Salesforce. Marketing got HubSpot. Post-sale got... spreadsheets + Slack + hope. Rocketlane proved that onboarding + implementation + PSA in one portal with AI agents is a $300M+ company. But they tax you: 5-seat minimum, $19-$99/user/mo + $29 AI add-on + custom Nitro credits, capped automations (50-500 runs/user/mo), closed agents, no self-host, no ERP connector, weak customization, slow at volume.

OpenLane does the same trifecta — **Projects + People/Profits + Customer Experience** — but open, self-hostable, API-first, BYO-LLM, unlimited automations, magic-link portal that customers actually use.

Principle: **PSA was built to track work. We are built to execute it — openly.**

---

## 3. ELI5 + GLOSSARY (NEWBIE START HERE)

Imagine you sell software to Adobe. Adobe pays. Now someone must install it, migrate data, train team, go live in 90 days. That someone is an **Implementation Manager**. They juggle 15 Adobes at once over email. Chaos.

OpenLane = one shared checklist + timeline + chat + files that BOTH your team and Adobe see, plus behind-the-scenes staffing (who works on what), time (hours logged), money (are we profitable?), and AI helpers that write docs, migrate data, chase late tasks.

Glossary (memorize these 20 words):

- **PSA:** Professional Services Automation. Fancy word for projects + resources + time + money in one tool.
- **Onboarding / Implementation:** taking customer from "signed" to "live."
- **Time-to-Value (TTV):** days from signature to customer gets value. Lower = better. Our North Star.
- **Playbook / Template:** reusable project blueprint. e.g., "Enterprise Onboarding v3."
- **Milestone / Phase:** big chunk. e.g., Kickoff, Migrate, Train, Go-Live.
- **Task:** smallest unit. Has owner (internal or customer), due date, status.
- **Customer Portal:** website customer sees. Branded as YOU, not us. No login via magic link.
- **Magic Link:** email link that logs you in without password. `click -> you're in for 7 days`.
- **Utilization:** % of time billable. 70% = healthy. 66.4% industry avg = bad.
- **Rate Card:** price per role per hour. e.g., Architect $200/hr.
- **Margin:** (revenue - cost)/revenue. Target 30-40%.
- **Revenue Recognition:** when you count money as earned (milestone vs time).
- **Resource:** human with skills + availability.
- **Capacity:** how many hours team has free.
- **CSAT / NPS:** happiness scores.
- **SOW:** Statement of Work. Contract saying what you'll do.
- **Scope Creep:** customer asks extra without paying. Killer.
- **Signal:** early warning. e.g., "customer hasn't logged in 7 days."
- **Agent:** AI that DOES work (migrates CSV, configures tenant), not just chats. Always needs human Approve.
- **RLS:** Row-Level Security. DB rule so Adobe never sees Nike's data.

If you understand above, you can build everything below.

---

## 4. WHO USES THIS? PERSONAS + REAL JOBS

### P0 Personas (build for them first)

1. **Asha, Implementation Manager (primary user)**
   - Manages 12 concurrent onboardings. Lives in OpenLane 6hrs/day.
   - Jobs: kickoff from template, assign customer tasks, chase late tasks, update status, log time, escalate risk.
   - Pain: "I spend 60% week chasing updates, not delivering."
   - Success: TTV down 50%, CSAT >8.5, no Sunday spreadsheet.

2. **Ravi, Customer Champion (external user)**
   - VP Ops at client. Busy, hates new tools. Checks portal 2x/week on phone.
   - Jobs: see "what's due for ME," upload file, approve milestone, see go-live date.
   - Pain: "Don't make me create another login."
   - Success: magic link works, 3 clicks to complete task.

3. **Meera, PS Leader / VP Delivery**
   - Owns 100 projects, margin, hiring.
   - Jobs: portfolio health, utilization heatmap, margin variance why?, forecast hiring.
   - Success: margin 35%, utilization 75%, at-risk flagged 2 weeks early.

4. **Dev, Solutions Architect / FDE**
   - Does migrations + configs.
   - Jobs: run migration, configure tenant via API, validate.
   - Success: 5 migrations in parallel, 50% less time.

5. **Ops Admin / CS Ops**
   - Sets templates, automations, integrations, SSO, audit.

### Secondary: Finance (invoicing), Sales (handoff), CSM (post-go-live), Partner (third-party SI).

### Real Companies Like Our ICP
Mid-market B2B SaaS 200-2500 staff, e.g., Intercom/Gong/Glean/Fivetran/GoCardless pattern: complex product, 30-90 day onboarding, Salesforce/HubSpot + Jira + Slack + Gong stack. Plus SIs/agencies (5-50 staff) implementing others' tools.

Anti-ICP (don't optimize yet): solopreneur, Fortune 500 multi-region with custom SAP, pure PLG with no human touch.

---

## 5. PRODUCT MAP — 9 PILLARS

```
[Sales Handoff] -> [Projects+Templates] -> [Tasks/Views] -> [Portal] -> [Docs/Files/Forms/Chat]
                        |                        |
                 [Resources+Time+Money] <-> [Agents/Nitro] <-> [Analytics/Signals]
                        |
                 [Integrations/API/Webhooks/MCP]
```

Build order: Projects -> Portal -> Tasks -> Docs/Files/Forms -> Time -> Resources -> Money -> Integrations -> Analytics -> Agents. Agents need data, so last, but design schema agent-ready day 1.

---

## 6. PILLAR 1: PROJECTS & TEMPLATES

### 6.1 WHAT
Project = one customer onboarding/delivery. Has customer, plan (phases/tasks), team, dates, health, money link, portal link.

Template = reusable blueprint with variables. e.g., `{{customer.name}}`, `{{plan.tier}}`.

### 6.2 WHY
Without templates every project starts blank -> 300hrs. With templates Graphite did $30k onboarding in 26hrs. Templates = scale without headcount.

### 6.3 CORE FIELDS (Project)
`id (uuid), workspace_id, customer_id, name, template_id + template_version, status [draft|active|on_hold|at_risk|delayed|completed|cancelled], health [green|amber|red|unknown + reason], owner_id, team_ids[], start_date, target_go_live, actual_go_live, progress_% (computed), budget_hours, billing_type, external_refs {salesforce_opportunity_id, hubspot_deal_id}, visibility {internal_only_fields[]}, created_from {manual| crm| api| agent}, created_at...`

Rules:
- Name required, 3-120 chars. Duplicate names allowed but warn if same customer + active.
- Dates: start <= target_go_live. If target changes, log history + reason required if shift >7 days.
- Status transitions guarded: draft->active needs owner + start_date + >=1 phase. active->completed needs all required tasks done or waived with reason + timesheets submitted + CSAT sent. Never delete completed, only archive.
- Every change writes `project_activity` log (who, what, old->new, source UI/API/agent/import).

### 6.4 TEMPLATES
Fields: `id, name, version (semver), category [onboarding|migration|implementation|support], phases[] with tasks[], task_defaults {owner_role, due_offset_days, required?, customer_visible?}, forms[], docs[], automations[], variables[], is_active, usage_count`.

Features newbie must build:
- Create project from template in <30sec: pick template -> fill 5 vars -> preview Gantt -> Create. Auto-assigns owners by role + availability (or placeholder).
- Dynamic templating: IF plan.tier==Enterprise THEN add SSO phase + 2 tasks. IF region==EU THEN add DPA form.
- Versioning: editing template does NOT break live projects. Live projects pin version. Show "Update available" banner with diff + one-click migrate (only for not-started tasks).
- Clone + share: duplicate project as new template. Export/import JSON.

Edge cases (handle all):
1. Template deleted but 20 projects use it -> soft-delete, block new use, keep pinned version readable.
2. Due_offset lands on weekend/holiday -> shift to next working day per workspace calendar, mark with 🗓️.
3. Owner role has zero available humans -> create with Unassigned + signal "staffing gap" + suggest backfill.
4. CRM handoff missing fields (no go-live date) -> create draft + task "Fill missing handoff" to sales owner, don't fail silently.
5. Customer name with emoji / 200 chars / RTL -> truncate display, keep full in DB, slug safe.
6. Bulk create 50 projects via CSV/API -> queue job, partial success report, idempotency key prevents dupes on retry.
7. Archiving project hides from default lists but keeps portal read-only for 90 days, then link shows "archived, contact owner."
8. Timezone: store UTC, display per user + per project default. Due 5pm IST != 5pm PST. Always show tz chip.

API: `POST /projects/from-template {template_id, customer_id, variables, start_date}` -> 201 + job_id if large. `GET /projects?status=active&owner=me&health=red`. `POST /templates/:id/new-version`.

Tests: create from template <2s for <200 tasks; version migrate doesn't touch completed tasks; permission: only admin can publish template.

---

## 7. PILLAR 2: CUSTOMER PORTAL (MAKE OR BREAK)

### 7.1 WHAT
External website per customer (or per project) branded as vendor: logo, colors, custom domain `onboarding.acme.com`. Customer sees ONLY what you share: their tasks, milestones, files, status updates, approvals, CSAT.

### 7.2 WHY
This is why people pick us over Asana. Internal tool guest seats confuse customers. Portal drives accountability + CSAT + faster TTV (GoCardless +59%).

### 7.3 GOLDEN RULES (copy these exactly)
1. **No login required by default.** Magic link email -> 7-day session. Optional OTP for sensitive. Full account creation is FRICTION and kills adoption (OnRamp mistake vs GUIDEcx win).
2. **3-click task:** open link -> see "Due for you (3)" -> complete/upload/approve.
3. **Never leak internal:** internal notes, margins, utilization, other customers NEVER queryable. Enforce at DB RLS + API + search.
4. **Mobile-first:** Ravi checks on phone. All actions work one-handed, offline-tolerant.
5. **Actionable emails:** customer can reply/complete task directly from email without opening portal (e.g., `[Approve] [Upload]` buttons with signed tokens).

### 7.4 FEATURES
- Branded header, progress bar, go-live countdown, milestones timeline.
- My Tasks vs All Tasks toggle. Filters: Due, Overdue, Done.
- File upload drag-drop + virus scan + 100MB default (configurable to 2GB) + preview (pdf/img/video).
- Approvals: Approve/Request changes with comment + e-sign option (Phase 2).
- Status updates feed (weekly digest auto-generated).
- Chat-lite per task + @mention customer (email push).
- CSAT widget at milestone: 😞😐😊 + comment. 1-click.
- Language: EN first, i18n keys from day 1.
- Accessibility: WCAG AA, keyboard nav, contrast, screen-reader labels.

Edge cases:
1. Magic link forwarded to wrong person -> link bound to email + single-use refresh rotation + "Not you? Request new link." + audit log IP.
2. Link expired -> friendly page with Resend button (rate-limited 5/hr/email) + owner notified after 3 fails.
3. Customer uploads .exe / 5GB / 1000 files -> block exe, chunked upload with resume, quota per project (default 10GB), show quota bar.
4. Customer completes wrong task -> allow Undo within 24h + log.
5. Two customer contacts edit same form -> last-write-wins with conflict banner + version history.
6. Customer never opens portal (50% do per Reddit) -> auto-nudge sequence (Day 2 email, Day 4 Slack if connected, Day 7 escalate to owner + signal). Track open/click.
7. Custom domain SSL fails -> fallback to `*.openlane.cloud` + alert admin, never 404.
8. Right-to-be-forgotten: delete customer user erases PII but keeps anonymized task history for reporting.
9. Screenshots with PII in portal -> disable indexing (`noindex`), signed URLs expire in 15min.
10. Offline: queue actions, sync on reconnect, show "pending."

API: `POST /portal-links {project_id, email, expires_in_days}` -> signed URL. `GET /p/:token` resolves with minimal data. All portal APIs use `portal_token` scope, never user JWT.

Tests: penetration test IDOR (customer A cannot fetch project B by guessing uuid); load 500 concurrent portal views; magic link brute-force rate limit.

---

## 8. PILLAR 3: TASKS, DEPENDENCIES, VIEWS

### 8.1 Task model
`id, project_id, phase_id, title, description_md, owner_type [internal|customer|partner|agent], owner_id, status [todo|in_progress|blocked|review|done|waived], priority, due_at, estimate_hours, required? (blocks completion), customer_visible?, dependencies[] (finish-to-start + lag), attachments[], comments[], custom_fields{}, created_by, completed_at/by, waived_reason`.

Rules:
- Title required, 255 max. Description markdown with @mentions + file embeds.
- Required tasks block phase/project completion. Waive needs reason + approver (owner's manager).
- Due date required for customer-visible tasks. If no duration and phase moves, tasks WITHOUT duration do NOT auto-move (Rocketlane bug report — we fix: prompt "move or keep?").
- Status machine: todo->in_progress->review->done. blocked from any. done is terminal unless reopened (logs reason).
- Bulk actions: assign, move phase, shift dates, mark done (max 500 at once, background job beyond).

### 8.2 Dependencies + Gantt
- Finish-to-start + lag days. Auto-shift dependents when predecessor slips, respecting work calendar. Show critical path in red.
- Cycle detection: block creation that creates loop, show path.
- Baseline: snapshot plan at kickoff. Show variance (planned vs actual) per milestone.

### 8.3 Views (all live from same data)
- List, Kanban (by status/owner), Gantt (with deps), Calendar, All Tasks (cross-project for Asha), Portfolio (for Meera).
- Filters saved per user. Shareable URLs preserve filters.
- Performance: virtualize lists >500 tasks. Gantt renders <1s for 1000 tasks (canvas, not DOM).

Edge cases: recurring tasks, subtasks (2 levels max to avoid hell), task templates inside phases, drag-drop with touch, undo bulk (snapshot + restore job), timezone due 11:59pm, daylight saving shift.

---

## 9. PILLAR 4: DOCS, FILES, FORMS, SHEETS, CHAT

Build as one "Spaces" surface, not 5 tools.

- **Docs:** collaborative editor (Yjs/Hocuspocus), templates (SOW, BRD, handoff), version history, traceability (link doc section to task/call). Export PDF.
- **Files (VaultS3):** S3-compatible via VaultS3 single binary (<80MB RAM). Use native versioning + diff/rollback, per-bucket AES-256-GCM encryption with rotation + crypto-shredding, presigned URLs (15min), multipart upload + resume, IAM policies + OIDC, lifecycle rules, notifications (for virus-scan + preview workers), built-in dashboard, FUSE mount for local dev. Virus scan via VaultS3 built-in scanner (fallback ClamAV worker), preview worker, dedupe by SHA-256, quota per project.
- **Forms:** drag-drop builder (text, date, file, e-sign Phase2), conditional logic (`if tier==Enterprise show SSO fields`), required fields ENFORCED (Rocketlane gap), prefill from CRM, response table + CSV.
- **Sheets:** lightweight grid for migration mapping (NOT Google Sheets clone). Column types, validation, 10k rows max per sheet (beyond -> use Migration Agent dataset). Row expand like Sheets (fix Rocketlane complaint).
- **Chat:** per project + per task threads, @mentions, email-to-chat gateway, Slack mirror (two-way). Never replace Slack, mirror it.

Edge: 100MB PDF preview OOM -> streaming render; concurrent doc edits conflict -> CRDT merge; form spam -> honeypot + rate limit; file name `../../../etc/passwd` -> sanitize; chat @all spam -> only owner can @all, rate-limited.

---

## 10. PILLAR 5: RESOURCES & CAPACITY

### WHAT
Who works on what, when, at what skill, at what cost.

Models:
- `person {id, name, role, skills[], cost_rate/hr, bill_rate/hr, capacity_hrs/week, timezone, time_off[]}`
- `allocation {project_id, person_id, role, hours, start/end, type [hard|soft], %}`
- Soft = tentative for pipeline deals. Hard = committed.

Features:
- Capacity heatmap (green/yellow/red), skills matrix filter, auto-suggest ("3 available Architects, pick Anu 80% free").
- Backfill flow: person on leave -> one-click find replacement with same skills + auto-reassign future tasks (keeps history).
- Demand forecast: sum pipeline (weighted by close %) vs capacity next 8 weeks -> hiring signal.
- Over-allocation guard: block >100% unless override with reason + manager approval.

Edge: part-time, contractor vs FTE cost, public holidays per country, parental leave, timezone overlap <2hrs warning, skill decay (don't assign Kubernetes if last used 2yrs ago without confirm), double-booking across workspaces.

Calc: utilization = billable_hours_logged / capacity. Exclude PTO, bench, internal. Show billable vs non-billable split. Month-lock snapshots (immutable after close).

---

## 11. PILLAR 6: TIME TRACKING

Make it invisible, not chore.

- Log via: task button (Start/Stop timer), calendar import (Google/Outlook one-click convert event -> timesheet), weekly grid, mobile, Slack `/log 2h @project`.
- Timesheet states: draft -> submitted -> approved/rejected. Approval chain: owner -> manager. Auto-reminders Fri 4pm + Mon 9am. Time Guardian agent flags gaps.
- Policies: min 8h/day? max 12h/day without overtime flag, no future >7 days, no logs on locked month.
- Expense sibling: receipts OCR Phase2.

Edge: timer left running 20hrs -> auto-pause after 8h + ask. Duplicate logs via calendar+manual -> dedupe suggest. Daylight saving 23hr day. Contractor in different currency. Backdated logs after invoice sent -> require finance approval + creates credit note draft.

---

## 12. PILLAR 7: MONEY (BUDGETS → INVOICING)

Flow: Estimate (SOW) -> Budget (hours x rate) -> Track (time x cost) -> Bill (invoice) -> Recognize (revenue) -> Margin report.

- **Budget:** per project/phase, hours + amount, alerts at 50/80/100% (+ Slack/email). Budgeted-hours alerts must be real-time, not nightly.
- **Rate Cards:** per workspace/client/role, versioned, currency, effective dates. e.g., Acme-2026: Architect $200, PM $150.
- **Billing:** T&M, fixed-fee milestone, retainer. Generate draft invoice from approved timesheets/expenses/milestones. Tax/VAT hooks, multi-currency with FX snapshot.
- **Invoicing:** PDF with logo, line items linked to time entries (auditable), QuickBooks/Xero sync, Stripe payment link Phase2.
- **Revenue Recognition:** milestone % vs time-proportional, month-locked snapshots for audit.
- **Margins:** real-time ` (billed - cost)/billed` per project/portfolio. Variance explainer: "Margin -8% because 20h overrun in Migration + contractor $180 vs $140."

Edge: rate change mid-project -> only future hours, past locked. Partial milestone (80% done) -> pro-rata or hold? Configurable. Refund/credit -> negative invoice linked. FX fluctuation -> lock rate at invoice date. Tax nexus per country. Un-invoiced hours >30 days -> red signal.

Never allow: editing locked month without CFO role + reason + audit; deleting invoice with payment; negative capacity.

---

## 13. PILLAR 8: INTEGRATIONS + API + WEBHOOKS + MCP

### Must-have V1 (else churn)
- Salesforce + HubSpot: Closed-won -> create project from mapped template. Bi-directional: project health/dates write back to Opportunity/Deal. Field mapper UI + test-run. Handle 1000s/day via queue.
- Jira/Linear: task sync two-way (status, assignee, comments). Conflict: Jira wins for eng fields, OpenLane wins for due date? Configurable + log.
- Slack: project channel auto-create, status digests, approve from Slack, `/openlane` commands.
- Gong/Zoom/Meet/Teams: ingest recordings -> Doc Agent.
- Calendar: timesheet assist.
- Zapier/Workato/Make via REST + webhooks. Native Workato recipe Phase2.
- Files: Google Drive/SharePoint link (not copy) Phase2.

### API-first rules (newbie: build API before UI)
- REST + OpenAPI 3.1, pagination (cursor), filtering, `Idempotency-Key` header for all POST, rate limit 1000/min/token with headers, version `/v1`.
- Every UI action hits same public API (dogfood).
- Webhooks: `project.created`, `task.completed`, `invoice.sent`, etc., HMAC signed, retries exp 5x, dead-letter queue + replay UI.
- MCP server: expose tools `create_project`, `suggest_resources`, `run_migration`, `get_health` so Claude/Workforce agents can call us. Auth via scoped tokens.
- Extension SDK (TS): custom apps inside portal (e.g., ROI calculator).

Edge: Salesforce outage -> queue + backoff, show "sync delayed" badge, never lose write (outbox pattern). Duplicate webhook delivery -> idempotent. Token leak -> instant revoke + rotate + audit. PII in logs -> redact.

---

## 14. PILLAR 9: ANALYTICS, SIGNALS, CSAT, PORTFOLIO

- Dashboards: My Work, Project 360, Portfolio (all projects health/TTV/CSAT/margin), Utilization, Profitability, Interval IQ (time between milestones to find bottlenecks).
- Signals engine: rules + ML. e.g., IF no customer login 7d AND 3 overdue customer tasks THEN red + suggest nudge. Every signal has evidence links, not just score.
- CSAT: at milestone + project end, 1-5 + comment, trend, response rate. Low score (<3) auto-creates escalation task to owner + manager.
- Reports: scheduled email/PDF, Snowflake export Phase2, custom builder (drag metrics).

Edge: survivorship bias (only happy reply) -> show response rate. Timezone week boundaries. Currency normalize to USD for portfolio. Drill-down must preserve filters.

---

## 15. OPEN NITRO — AGENTIC LAYER (KILLER)

Architecture: **Skills + Guardians + Approvals.** Agent never writes prod without human Approve + validation + audit.

Global guardrails:
- Scoped permissions (agent = acting user, can't exceed).
- Dry-run preview sheet first.
- Full audit: prompt, model, inputs hash, outputs, approver, diff.
- BYO-LLM: OpenAI/Anthropic/Ollama/Azure. Model router by task (cheap for nudges, smart for migration). Cost meter per workspace.
- Kill switch per workspace + per agent.
- Evals: golden datasets per agent, CI must pass >95% before deploy.

### 15.1 Documentation Agent
Input: Gong/Meet transcript + emails + CRM notes + template. Output: SOW/BRD/solution doc with every sentence cited `[call 12:34]`. Living doc auto-updates on new calls. Chat-with-doc.
Edge: hallucinated requirement (not in source) -> validator blocks + "uncited" badge. 79-page doc -> chunk + map-reduce. PII redaction. Language detection.

### 15.2 Migration Agent (our viral wedge)
Flow: upload source CSV/XLS -> pick destination schema (prebuilt for Salesforce/HubSpot/etc) -> agent suggests mapping + transforms in plain English -> preview 100 rows + validation errors -> Approve -> run full (background) -> versioned dataset + rollback -> audit.
Transforms: normalize phones E.164, dates, dedupe, cross-table reconcile (`every customer_id has transaction`), financial sum check.
Edge: 1M rows -> streaming + sample. Encoding hell (latin1 vs utf8) -> auto-detect. Formula injection (`=CMD`) -> sanitize. Partial fail -> quarantine rows + continue + report.

### 15.3 Workforce / Configuration Agent
Reads order form -> maps to product API calls via OpenAPI/MCP/browser-use adapter -> shows config sheet -> human Approve -> executes with retries -> writes audit back to task.
Edge: API rate limit -> backoff. Destructive action (delete env) -> require 2-person approval. Drift (config changed manually) -> detect + reconcile prompt.

### 15.4 Resourcing Agent
Suggests team from skills/availability/workload/context. Explains why. Handles extensions/backfills automatically.
Edge: bias (always picks same senior) -> diversity + load-balance penalty. Leave conflict -> block.

### 15.5 Governance + Time Guardian + Finance Guardian
Enforces playbooks: time policy, margin thresholds, required fields. Flags, doesn't auto-fix money without approval.

### 15.6 Signals + Assistant + Analyst
Signals: risk/opportunity with story. Assistant: preps QBR/steering deck + talk track. Analyst: NL query -> SQL (guarded, read-only, row-filtered) -> chart + "why" narrative.

All agents log to `agent_runs` table for billing transparency (we show cost, unlike Nitro black-box).

---

## 16. CROSS-CUTTING

- **Auth:** email+magic link+passwordless + Google/Microsoft SSO, SAML OIDC Enterprise, SCIM Phase2, 2FA TOTP/WebAuthn, session 30d internal / 7d portal, device list, instant revoke.
- **Permissions:** RBAC roles [owner|admin|manager|member|viewer|client|partner] + ABAC (project membership, customer_visible flag). RLS in Postgres enforces tenant + visibility. Every API checks.
- **Automations:** visual builder (when/condition/action), unlimited runs (our edge vs 50-500 cap), 500+ templates, run history + replay, dry-run. Triggers: task, date, CRM, webhook, schedule, signal.
- **Notifications:** matrix (who gets what via email/Slack/in-app/push). Digest to avoid fatigue. Quiet hours per user. Unsubscribe granular, never for assigned task overdue.
- **Search:** global (projects/tasks/docs/files) with permission filtering, typo-tolerant, recent first. Portal search scoped.

---

## 17. SECURITY BIBLE (SHIP OR DIE)

Newbie: do these before launch, no excuses.

1. **Tenancy isolation:** `workspace_id` on every table + RLS policies + tests that user A cannot read workspace B (automated IDOR fuzz).
2. **Portal isolation:** portal tokens are opaque, random 256-bit, scoped to single project + email, expiry + rotation, rate-limited (10 tries/IP/hr), logged.
3. **Secrets:** env via vault (Doppler/Infisical), never in git, rotate every 90d, separate keys per env, Stripe/Salesforce OAuth stored encrypted (AES-256-GCM + KMS).
4. **Crypto:** TLS 1.2+, HSTS, at-rest AES-256, bcrypt/argon2id for any passwords, signed URLs 15min.
5. **Injection:** parameterized queries only (pgx + sqlc, no string concat), go-playground/validator on every handler + bluemonday HTML sanitize, markdown render safe, CSV formula strip, file type sniff not extension, SVG disallow scripts.
6. **Auth:** brute-force lockout, breached-password check, session fixation regen, CSRF tokens for cookies, JWT short-lived + refresh rotation.
7. **SSO/SAML:** XML signature validation, audience check, JIT provisioning with default viewer role.
8. **Uploads:** VaultS3 built-in virus scan (ClamAV worker as fallback), 100MB default, quarantine bucket, content-disposition attachment.
9. **Webhooks:** HMAC verify, timestamp skew <5min, replay protection.
10. **AI safety:** PII redactor before LLM, no training on customer data without opt-in DPA, prompt-injection guard (system vs user separation, tool output quarantined), output validator (citations required), human approval for writes, data residency option (EU region + Ollama local).
11. **Compliance:** SOC2 logs (immutable audit trail 7yrs), GDPR delete/export (30d SLA), DPA, subprocessor list, backup encryption + restore drills.
12. **Headers:** CSP, X-Frame (portal embed allowlist), CORS allowlist, Referrer-Policy.
13. **Rate limits + WAF + DDoS:** Cloudflare, per-IP + per-token buckets, file/IP blocklist.
14. **Supply chain:** lockfile, Dependabot, SAST (CodeQL/Semgrep), container scan, SBOM.
15. **Incident:** runbook, status page, 24h breach notify draft.

Test: `npm run sec:check` must pass: IDOR suite, XSS payloads, SQLi payloads, portal escape, webhook forgery.

---

## 18. SCALABILITY + RELIABILITY BIBLE

Targets (V1): 10k workspaces, 100k projects, 2M tasks, 500 concurrent portal users, p95 API <300ms, Gantt 1k tasks <1s, 99.9% uptime, RPO 5min RTO 30min.

How:

- **Arch:** modular Go monolith first (Go Chi API + Go River worker + Next.js web). Split to services only when pain (billing worker, agent worker). Monorepo `apps/web, apps/api, apps/worker, db/, packages/contracts/`.
- **DB:** Postgres 16, indexes on (workspace_id, status), (project_id, due_at), GIN on search, partitioning `activity` by month, read replica for analytics, PgBouncer pooling, migrations with zero-downtime (expand-contract).
- **Cache:** Redis for sessions, portal tokens, heatmaps (TTL 60s), rate limits. No cache for money without invalidation on write.
- **Queues:** River (Postgres-backed) for imports, migrations, emails, webhooks, agent runs; Temporal Go SDK for agent workflows in P2. Idempotent jobs, retries exp + DLQ + dashboard.
- **Files:** VaultS3 + CDN, presigned PUT/GET (API never proxies bytes), multipart upload with resume, preview worker.
- **Realtime:** WS for chat/presence, polling fallback for portal.
- **Multi-region Phase2:** EU data residency, read replicas.
- **Observability:** OpenTelemetry traces, structured logs (no PII), metrics (RED), Sentry, uptime checks, SLO alerts (pager for p95>800ms 5min).
- **Backups:** PITR daily + WAL, cross-region copy, restore drill monthly, customer export (full JSON + files) self-serve.
- **Perf tricks:** virtualized lists, canvas Gantt, debounced search, cursor pagination, N+1 audit in CI.

Edge: thundering herd on Monday 9am reminders -> jitter + batch. Big import 1M rows OOM -> streaming + temp table. Tenant noisy neighbor -> per-workspace job concurrency cap.

---

## 19. TECH ARCHITECTURE + REPO LAYOUT + DB CORE

**Stack — LOCKED: Full Go backend (per decision):**
Frontend: Next.js 14 App Router + React + Tailwind + shadcn/ui + TanStack Query + Zustand. Talks to Go API only (no Next Route Handlers for business logic).
Backend API: Go 1.22+ + Chi (stdlib-compatible router) + pgx/v5 + sqlc (type-safe SQL) + goose (migrations) + go-playground/validator + oapi-codegen (OpenAPI 3.1 first). Auth: JWT (short) + rotating refresh + OIDC (Dex) + SAML via Dex Enterprise, TOTP/WebAuthn.
DB: Postgres 16 + RLS (Go sets `app.user_id` / `app.workspace_id` via `SET LOCAL`) + PgBouncer + pgvector for knowledge.
Queues/Workers: River (Postgres-based, transactional outbox, no extra infra for P0) -> Temporal Go SDK for agent workflows in P2. Redis kept only for rate-limit + cache + pub/sub (optional, can drop in P0).
Realtime: Go WS hub (Centrifuge-compatible) for presence/chat/notifications + tiny Node sidecar `yjs-ws` ONLY for doc collab (Yjs/Hocuspocus). Docs content still stored via Go API.
AI: Go agent runner (explicit state machines, not magic chains) calling OpenAI/Anthropic via official Go SDKs + pgvector retrieval. LangGraph/Python sidecar explicitly rejected for P0 to keep single binary story. Model router + cost meter built-in.
Infra: Docker Compose local (web + api + worker + postgres + redis + vaults3 + dex), single Go binaries for api/worker, Fly/Railway start, Helm later. CI GitHub Actions (golangci-lint + sqlc diff check + go test + IDOR suite).

**Object Storage — VaultS3 (replaces MinIO):**
Why: single binary, <80MB RAM vs MinIO 512MB+, no paid tier gate, no telemetry, built-in dashboard + IAM/OIDC + versioning with diff/rollback + erasure coding + Raft clustering + active-active replication + virus scanning + full-text/vector search + backup scheduler. S3 API compatible (SigV4, multipart, presigned URLs, tagging, lifecycle, CORS, bucket policies). Drop-in for AWS SDK / aws-cli / boto3.
Local: `docker run -p 9000:9000 eniz1806/vaults3` with `VAULTS3_ACCESS_KEY / VAULTS3_SECRET_KEY / VAULTS3_DATA_DIR=/data / VAULTS3_METADATA_DIR=/metadata`. Buckets: `openlane-files`, `openlane-previews`, `openlane-backups` with versioning ON, encryption ON, lifecycle (previews 90d, multipart abort 7d).
Prod: VaultS3 clustered (3 nodes Raft) + erasure coding + nightly scheduled backups to R2/S3 + active-active replication if multi-region. App uses presigned PUT/GET only, never proxies bytes (except virus-scan worker). Quota enforced in API + bucket policy.

```
openlane/
  apps/web/ (Next.js portal + app, no business logic)
  apps/api/ (Go Chi REST + webhooks + MCP, cmd/api/main.go)
  apps/worker/ (Go River jobs + agent runners, cmd/worker/main.go)
  db/ (goose migrations + sqlc queries + RLS policies + seeds)
  packages/contracts/ (OpenAPI 3.1 YAML, source of truth, generates TS client + Go server)
  infra/docker/ infra/helm/
  docs/ (this bible + ADRs)
  e2e/ (Playwright)
```

Core tables (simplified): workspaces, users, memberships, customers, contacts, projects, phases, tasks, dependencies, templates, docs, files, forms/responses, sheets, time_entries, allocations, budgets, rate_cards, invoices, signals, notifications, automations/runs, agent_runs, audit_logs, portal_links, integrations/connections.

Every table: `id uuid pk, workspace_id, created_at, updated_at, created_by, deleted_at (soft)`. Never hard-delete money/audit.

---

## 20. UI/UX DESIGN SYSTEM

Tokens: --bg, --surface, --border, --text, --muted, --primary (indigo #4F46E5), --success/--warn/--danger, radius 10px, shadow-sm/md, font Inter + mono JetBrains.

Components: Button (primary/secondary/ghost/danger + loading), Input+error, DatePicker with tz chip, Assignee pill with avatar+role, Health dot + tooltip reason, Progress bar with variance, Empty states with CTA (never blank), Command-K palette, Toasts with Undo.

Portal theme: customer logo, primary color auto-contrast-checked (AA), custom domain, no OpenLane branding on Pro (white-label).

Motion: 150ms ease, skeleton loaders, optimistic updates with rollback toast.

Mobile: bottom nav for portal (Home/Tasks/Files/Chat), swipe complete, haptic.

---

## 21. 40 BETTER FEATURES (BEYOND ROCKETLANE — OUR MOAT)

1. Unlimited automations (no credit tax)
2. BYO-LLM + local Ollama mode (data never leaves VPC)
3. Open Migration Playbooks marketplace (share mappings)
4. One-click Salesforce/HubSpot field-map tester
5. Actionable email (approve from inbox, no login)
6. WhatsApp/SMS nudges for customer tasks (emerging markets win)
7. Voice notes -> tasks (mobile)
8. Required fields + timestamped notes (fix their top complaints)
9. Row expand in Sheets + 10k rows smooth
10. Offline portal + queue sync
11. Public roadmap portal per customer
12. E-sign built-in (no DocuSign tax)
13. Video Loom embed + transcript -> doc
14. Auto RACI generator from template
15. Scope-creep detector (diff SOW vs tasks, suggests change order draft)
16. Change-order one-click (extra $$, e-sign)
17. Pricing calculator linked to time benchmarks (Graphite use-case built-in)
18. Hiring forecast ("hire 2 PMs by Oct or 5 projects slip")
19. Bench optimizer (who's free + best fit)
20. Margin "why" explainer in plain English
21. Un-invoiced radar + one-click invoice
22. Client health score blending portal engagement + sentiment + timeline
23. Stakeholder map (who's ghosting, who champions)
24. Pre-sales mode (share plan template as sales asset, converts to project on Closed-won)
25. Partner portal (SI gets scoped view, no full seat)
26. SOC2 starter kit (audit export one-click)
27. Data residency picker (US/EU/IN) + self-host
28. Full export (JSON + files) anytime, no lock-in (trust moat)
29. MCP-first (every feature callable by Claude/Agent PM)
30. Agent cost dashboard (show $ per run, vs Nitro black-box)
31. Prompt studio (edit agent prompts per workspace, versioned, A/B)
32. Golden evals per agent in CI
33. Browser-use recorder (record clicks once -> Workforce replays)
34. API call sandbox (test product API before agent runs)
35. Time-from-calendar with one-click + dedupe
36. Calendar capacity overlay (PTO + meetings = true availability)
37. Multi-language portal (EN/HI/ES/DE day 1 keys)
38. Dark mode + high-contrast + keyboard-only flows
39. Transactional SMS fallback when email bounces
40. Community templates hub (like Vercel templates, stars + forks)

Pick 5 for V1 wow: 1,2,5,8,15.

---

## 22. EDGE-CASE ENCYCLOPEDIA (BUILD CHECKLIST — TICK THESE)

**Projects:** duplicate names, blank template, template v drift, bulk 500, archive with active portal, timezone DST, delete owner, merger (two projects same customer), leap-year due Feb29.

**Portal:** forwarded link, expired, brute force, XSS in customer name, 5GB upload, exe, quota full, undo complete, concurrent form edit, never-login customer, custom domain SSL fail, GDPR delete, signed URL leak, bot scraping.

**Tasks:** cycle dep, no-duration move, 10k tasks import, subtask depth, reopen done, bulk undo, @mention ex-employee, due in past, estimate 0h, required waive without approver.

**Resources:** zero candidates, over-allocate override, contractor cost change mid-sprint, holiday calendar per country, skill typo, leave overlapping kickoff, soft->hard race (two PMs claim same person).

**Time:** timer 20h, future log, locked month edit, duplicate calendar, 23hr DST day, currency convert, backdate after invoice.

**Money:** rate change mid-project, partial milestone, refund, FX swing, tax, un-invoiced 90d, negative margin alert fatigue.

**Integrations:** Salesforce down, field deleted, duplicate webhook, token revoked mid-sync, rate limit 429, large payload 50MB, PII in logs.

**Agents:** hallucinated mapping, 1M rows OOM, API 500 mid-run, destructive delete, cost spike ($200 run), prompt injection in customer file ("ignore instructions"), offline LLM, approval timeout 7d.

**Security:** IDOR, JWT replay, CSRF on magic-link redeem, SAML sig bypass, RLS bypass via search, CSV injection, SSRF via webhook URL or file fetch, open redirect after login.

Each needs: repro steps + expected + test id (e.g., EC-PORTAL-07). Add to Playwright + unit.

---

## 23. BUILD ROADMAP P0-P3 + ACCEPTANCE

**P0 MVP (6-8 weeks, 2 devs + AI): Goal — one team runs 5 real onboardings.**
- Auth + workspaces + RBAC + audit
- Projects from templates (versioned) + phases/tasks + Gantt/List/Kanban + deps + baselines
- Portal magic-link + my tasks + files + approvals + CSAT + actionable email
- Docs/files/forms (required fields!) + chat-lite
- Time simple + Salesforce/HubSpot one-way -> project create + Slack notify
- Notifications + search + automations v1 (unlimited)
- Docker Compose + backups + basic observability
Accept: TTV tracked, portal open rate >60%, create project <30s, p95 <400ms, IDOR suite green.

**P1 PSA (8 weeks):** resources/capacity/skills, time approvals + calendar, budgets/rate cards/invoicing/QuickBooks, Jira two-way, portfolio + utilization + margin dashboards, required fields/timestamped notes, custom domain, SCIM/SSO.

**P2 Open Nitro (8 weeks):** Doc Agent + Migration Agent + Signals + Resourcing suggest + Governance. Approval gates + audit + cost dashboard + evals. MCP server.

**P3 Enterprise (ongoing):** Workforce config agent (API/MCP/browser), Analyst NL->SQL, multi-currency/rev rec, Snowflake export, EU region, e-sign, WhatsApp, marketplace, SOC2 Type II kit, 99.99%.

Definition of Done per feature: UI + API + RLS + tests (unit+IDOR+e2e) + docs + analytics event + audit log + portal check (does customer see too much?).

---

## 24. OPEN-SOURCE + PRICING STRATEGY

License: **AGPL-3.0 core** (forces contributions, blocks closed SaaS copy) + commercial Enterprise for SSO/SCIM/audit retention/data residency/support. Alternative BSL if you fear AWS clone (like Cannelle). Never MIT for core (else Salesforce clones closed).

Pricing to undercut:
- Community: $0 self-host, unlimited projects/customers, unlimited automations, community Discord.
- Cloud Starter: $15/user/mo (vs $19 but no seat min, no AI tax) — 1-10 users.
- Cloud Pro: $39/user/mo — everything + agents + Salesforce/Jira + custom domain.
- Enterprise: custom — SSO, EU, SLA, dedicated.
Agents: include 1000 runs/mo, then transparent $0.02/run + LLM passthrough (vs Nitro black-box credits).

GTM: launch on HN + Reddit r/CustomerSuccess with "We open-sourced Rocketlane + unlimited automations" + migration importer (1-click from Rocketlane CSV/GUIDEcx/Asana) + template marketplace.

---

## 25. WHAT TO BUILD FIRST TOMORROW MORNING

1. `git init openlane` monorepo + Docker (Next.js + Postgres RLS + Redis + VaultS3) + Auth + workspace.
2. Project + Template CRUD + version pin + Gantt (build API first).
3. Portal magic-link vertical slice: create link -> view tasks -> complete task -> audit. Get 1 real customer to click.
4. Ship importer: Rocketlane/Asana CSV -> OpenLane. This steals users.
5. Then time + Slack + Salesforce create.

Track in `TASKS.md` (generate from §22 IDs). Every PR must link EC-IDs it fixes.

---

### Final Note to Future Us

Rocketlane won by turning chaos into a portal + templates + time/money truth + agents that execute. We win by doing same OPENLY, cheaper, faster, with customers who actually click the link.

Trust the process. Handle every edge. Instrument TTV. Stay boring on security, magical on portal.

Let's prove ourselves. Ship P0.

— OpenLane Bible v1.0
