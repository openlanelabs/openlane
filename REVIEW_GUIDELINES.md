# OpenLane Project Review Guidelines

These are appended to the general rubric. They override nothing; they add project-specific signal.

## Security invariants (flag ANY violation as P0)
- Every handler/worker touching tenant tables must open a tx and run `set_config('app.workspace_id', ...)` BEFORE any query — `set_config(..., true)` on a bare pool connection dies with the implicit tx (each pool.Exec is its own tx). Reads too.
- Hand-constructed JSON strings are banned — use `mustJSON()` (CodeQL flags concat JSON as critical).
- User-supplied URLs are never fetched in-process except via the worker dial-time guard (`net.Dialer{Control}` checking the connected IP — survives DNS rebinding). Config-typed inputs still get the allowlist.
- Secrets (PATs, API keys) are sealed with `sealSecret`/`sfSecretAEAD` before storage and never selected back into responses or logs.
- LLM output never executes: agents pick from catalogs/slots (Analyst), validated mappings (Migration), or produce markdown (Doc) — any new agent route must keep the LLM out of the execution path. Flag any free-SQL or eval-shaped use.

## Agent (Nitro) rules
- Every agent route: manager+ role gate, workspace kill-switch check (503), metered `agent_runs` row (model + cost_cents + input/output refs), and agents suggest while humans commit (no autonomous writes to customer-visible state).
- Evidence contracts are structural: signals/rules must carry evidence (ids/counts) in jsonb, not just scores.

## Data-shape traps (known gotchas — check migrations carefully)
- `time_entries.minutes` CHECK caps at 720 and rejects 0; `tasks` has no assignee column; `portal_links.contact_id` NOT NULL; `rate_cards` is a header table (rates live in `rate_card_rates`).
- Narrowing CHECKs in down migrations needs DELETE/UPDATE guards first.
- New tenant tables: RLS ENABLE + FORCE + `tenant_isolation` NULLIF policy + grants to `openlane_app`; junction-table deletes may need explicit `GRANT DELETE`.

## Tests
- Integration tests run against real Postgres; new handlers need a test using the established stacks (filesTestStack/timeTestStack/etc). Runtime smoke through the web proxy (with OPENLANE_API_URL pinned) is required for web-touching PRs — the #109/#123 lessons.
- The RLS suite is single-shot on a fresh DB and must run as `openlane_app`, never the owner.

## Style
- Conventional commits; co-author trailer required. Staticcheck is strict in CI (QF1012, S1011, SA9003 have all fired) — check obvious lint before pushing.
