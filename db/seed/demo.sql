-- OpenLane demo seed — DEMO ONLY. Never run against a real database.
-- Usage (after migrations, as DB owner, repo root):
--   psql "$DATABASE_URL" -f db/seed/demo.sql
-- Idempotent: TRUNCATE first, so re-running always converges to the same
-- state. Fixed UUIDs keep demo URLs stable. The portal token below is
-- DETERMINISTIC on purpose — anyone with this repo can mint this link,
-- which is fine for a demo and exactly why it must never touch prod.

TRUNCATE audit_logs, portal_links, tasks, projects, contacts, customers, workspaces, users, memberships CASCADE;

INSERT INTO workspaces (id, name, slug) VALUES
  ('11111111-1111-1111-1111-111111111111', 'Acme SI', 'acme');

INSERT INTO users (id, email, display_name) VALUES
  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha Verma');

SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

INSERT INTO customers (id, workspace_id, name) VALUES
  ('33333333-3333-3333-3333-333333333333', '11111111-1111-1111-1111-111111111111', 'Adobe');

INSERT INTO contacts (id, workspace_id, customer_id, email, display_name, is_portal_user) VALUES
  ('44444444-4444-4444-4444-444444444444', '11111111-1111-1111-1111-111111111111',
   '33333333-3333-3333-3333-333333333333', 'ravi@adobe.test', 'Ravi Kumar', true);

INSERT INTO projects (id, workspace_id, customer_id, name, status, start_date, target_go_live) VALUES
  ('55555555-5555-5555-5555-555555555555', '11111111-1111-1111-1111-111111111111',
   '33333333-3333-3333-3333-333333333333', 'Adobe Onboarding', 'active',
   CURRENT_DATE - 7, CURRENT_DATE + 21);

INSERT INTO tasks (id, workspace_id, project_id, title, owner_type, customer_visible, status, due_at, completed_at) VALUES
  -- customer-visible: one overdue, one due soon, one with headroom
  ('c0000000-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Upload employee CSV (HRIS export)', 'customer', true, 'todo', now() - interval '2 days', NULL),
  ('c0000000-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Approve kickoff scope (SOW v2)', 'customer', true, 'todo', now() + interval '2 days', NULL),
  ('c0000000-0000-0000-0000-000000000003', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Nominate 3 portal champions', 'customer', true, 'todo', now() + interval '9 days', NULL),
  -- internal: never rendered in the portal (RLS enforces customer_visible)
  ('c0000000-0000-0000-0000-000000000004', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Internal: margin review with Meera', 'internal', false, 'todo', NULL, NULL),
  ('c0000000-0000-0000-0000-000000000005', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Internal: sandbox provisioning', 'internal', false, 'done', NULL, now() - interval '1 day'),
  ('c0000000-0000-0000-0000-000000000006', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
   'Internal: weekly digest auto-draft', 'internal', false, 'todo', NULL, NULL);

-- DEMO ONLY: deterministic 43-char base64url token
-- (nAKHcRHg8meLXNaD6Q9sG8V2GkhbUI6pL9FRVb3gAJs) so `make demo` can print
-- a clickable URL. Hash-only in DB, like every link. Must satisfy the same
-- shape the API enforces (minLength 43) or the demo 401s.
INSERT INTO portal_links (id, workspace_id, project_id, contact_id, token_hash, status, expires_at)
VALUES ('d0000000-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111',
  '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444',
  encode(sha256('nAKHcRHg8meLXNaD6Q9sG8V2GkhbUI6pL9FRVb3gAJs'::bytea), 'hex'), 'active', now() + interval '7 days');
