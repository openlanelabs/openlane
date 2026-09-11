-- Portal RLS + IDOR acceptance suite (issues #20-#22, EC-PORTAL-07).
--
-- Runs AS openlane_app (non-owner — RLS applies; owners/superusers bypass it).
-- Single-shot on a fresh migrated DB (CI resets per run). set_config with
-- is_local=false persists for the session; each group re-sets its context.
-- Any FAIL row raises → non-zero exit → CI red.
--
-- Usage:
--   psql "postgres://openlane_app:PW@/openlane_test" -v ON_ERROR_STOP=1 -f db/test/portal_rls_suite.sql

\set ON_ERROR_STOP on

CREATE TEMP TABLE test_results (name text, ok boolean);

-- ---------- seed (tenant context, as the API would) ----------
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);
INSERT INTO customers (id, workspace_id, name) VALUES
  ('33333333-3333-3333-3333-333333333333', '11111111-1111-1111-1111-111111111111', 'Adobe');
INSERT INTO contacts (id, workspace_id, customer_id, email, display_name, is_portal_user) VALUES
  ('44444444-4444-4444-4444-444444444444', '11111111-1111-1111-1111-111111111111', '33333333-3333-3333-3333-333333333333', 'ravi@adobe.test', 'Ravi', true);
INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES
  ('55555555-5555-5555-5555-555555555555', '11111111-1111-1111-1111-111111111111', '33333333-3333-3333-3333-333333333333', 'Adobe Onboarding', 'active'),
  ('66666666-6666-6666-6666-666666666666', '11111111-1111-1111-1111-111111111111', '33333333-3333-3333-3333-333333333333', 'Adobe Phase 2', 'active');
INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, due_at) VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Upload employee CSV', 'customer', true, now() - interval '1 day'),
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Approve kickoff scope', 'customer', true, now() + interval '3 day'),
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Internal migration plan', 'internal', false, null),
  ('11111111-1111-1111-1111-111111111111', '66666666-6666-6666-6666-666666666666', 'Phase 2 SSO setup', 'customer', true, null);
INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at) VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444',
   encode(sha256('suite-token-a'::bytea), 'hex'), now() + interval '7 days');

-- ---------- T1: tenant isolation ----------
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);
INSERT INTO test_results
SELECT 'T1a tenant sees own projects', count(*) = 2 FROM projects;

SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T1b cross-tenant sees nothing (IDOR)', count(*) = 0 FROM projects;

SELECT set_config('app.workspace_id', '', false);
INSERT INTO test_results
SELECT 'T1c no context sees nothing', count(*) = 0 FROM projects;

-- ---------- T2: portal scope (EC-PORTAL-07) ----------
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T2a portal sees its customer-visible tasks only', count(*) = 2
FROM tasks WHERE project_id = '55555555-5555-5555-5555-555555555555';
INSERT INTO test_results
SELECT 'T2b portal cannot see other project (IDOR)', count(*) = 0
FROM tasks WHERE project_id = '66666666-6666-6666-6666-666666666666';
INSERT INTO test_results
SELECT 'T2c portal never sees internal tasks', count(*) = 0
FROM tasks WHERE customer_visible = false;
INSERT INTO test_results
SELECT 'T2d portal project scope is exactly one project', count(*) = 1 FROM projects;

-- ---------- T3: portal completion ----------
UPDATE tasks SET status = 'done', completed_at = now()
WHERE project_id = '55555555-5555-5555-5555-555555555555' AND customer_visible AND status = 'todo';
INSERT INTO test_results
SELECT 'T3a portal completes its tasks', count(*) = 2
FROM tasks WHERE completed_at IS NOT NULL;

UPDATE tasks SET status = 'done', completed_at = now() WHERE customer_visible = false;
INSERT INTO test_results
SELECT 'T3b portal cannot complete internal tasks', count(*) = 0
FROM tasks WHERE customer_visible = false AND completed_at IS NOT NULL;

-- ---------- T4: portal audit append ----------
INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, new_value, source)
VALUES ('11111111-1111-1111-1111-111111111111', 'task', '55555555-5555-5555-5555-555555555555',
        'contact', '44444444-4444-4444-4444-444444444444', 'task.completed', '{"status":"done"}'::jsonb, 'portal');
INSERT INTO test_results SELECT 'T4a portal audit row appends', true;

-- T4b: foreign-workspace audit insert must be BLOCKED by RLS (expected
-- insufficient_privilege); if it ever succeeds, the test fails loudly.
DO $check$
BEGIN
  BEGIN
    INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
    VALUES ('22222222-2222-2222-2222-222222222222', 'task', '55555555-5555-5555-5555-555555555555',
            'contact', '44444444-4444-4444-4444-444444444444', 'task.completed', 'portal');
    RAISE EXCEPTION 'T4b FAIL: foreign-workspace audit insert was NOT blocked by RLS';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;  -- blocked as expected (SQLSTATE 42501)
  END;
END
$check$;
INSERT INTO test_results
SELECT 'T4b portal audit foreign-workspace blocked', count(*) = 0
FROM audit_logs WHERE workspace_id = '22222222-2222-2222-2222-222222222222';

-- ---------- T5: revocation kills access ----------
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);
UPDATE portal_links SET status = 'revoked'
WHERE token_hash = encode(sha256('suite-token-a'::bytea), 'hex');
SELECT set_config('app.workspace_id', '', false);
INSERT INTO test_results
SELECT 'T5 revoked token sees nothing', count(*) = 0
FROM tasks WHERE project_id = '55555555-5555-5555-5555-555555555555';


-- ---------- T6: templates tenant isolation (issue #35) ----------
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);
INSERT INTO templates (id, workspace_id, name, category, phases) VALUES
  ('77777777-7777-7777-7777-777777777777', '11111111-1111-1111-1111-111111111111', 'Enterprise Onboarding', 'onboarding',
   '[{"name":"Kickoff","tasks":[{"title":"Sign SOW","due_offset_days":2,"required":true,"customer_visible":true}]}]');
INSERT INTO test_results
SELECT 'T6a template created in tenant', count(*) = 1
FROM templates WHERE id = '77777777-7777-7777-7777-777777777777';

-- beta tenant cannot see acme's templates
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T6b cross-tenant template invisible', count(*) = 0
FROM templates WHERE id = '77777777-7777-7777-7777-777777777777';

-- beta cannot insert a template claiming acme's workspace
DO $check$
BEGIN
  BEGIN
    INSERT INTO templates (workspace_id, name, category, phases)
    VALUES ('11111111-1111-1111-1111-111111111111', 'Stolen', 'support', '[]');
    RAISE EXCEPTION 'T6c FAIL: cross-tenant template insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;
INSERT INTO test_results
SELECT 'T6c cross-tenant insert blocked', count(*) = 0
FROM templates WHERE name = 'Stolen';

-- no-context sees nothing
SELECT set_config('app.workspace_id', '', false);
INSERT INTO test_results
SELECT 'T6d no-ctx template invisible', count(*) = 0
FROM templates;

-- restore tenant ctx for cleanliness
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- T7: files (00008) ----------
-- customer 3333…(Adobe) belongs to ws 1111; project 5555… is theirs.
INSERT INTO files (id, workspace_id, project_id, name, object_key, content_type, size_bytes, status, customer_visible)
VALUES
  ('f1111111-1111-1111-1111-111111111111', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'kickoff.pdf', '11111111-1111-1111-1111-111111111111/55555555-5555-5555-5555-555555555555/aaaaaaaa-1111-1111-1111-111111111111', 'application/pdf', 1024, 'uploaded', true),
  ('f2222222-2222-2222-2222-222222222222', '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'internal-notes.md', '11111111-1111-1111-1111-111111111111/55555555-5555-5555-5555-555555555555/bbbbbbbb-2222-2222-2222-222222222222', 'text/markdown', 512, 'uploaded', false);
-- ws 2222: own customer + project + file (under ITS ctx so WITH CHECK passes)
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO customers (id, workspace_id, name) VALUES
  ('77777777-7777-7777-7777-777777777777', '22222222-2222-2222-2222-222222222222', 'Gamma Inc');
INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES
  ('88888888-8888-8888-8888-888888888888', '22222222-2222-2222-2222-222222222222', '77777777-7777-7777-7777-777777777777', 'Gamma Setup', 'active');
INSERT INTO files (id, workspace_id, project_id, name, object_key, content_type, size_bytes, status, customer_visible)
VALUES
  ('f3333333-3333-3333-3333-333333333333', '22222222-2222-2222-2222-222222222222', '88888888-8888-8888-8888-888888888888', 'theirs.txt', '22222222-2222-2222-2222-222222222222/88888888-8888-8888-8888-888888888888/cccccccc-3333-3333-3333-333333333333', 'text/plain', 256, 'uploaded', false);
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

INSERT INTO test_results
SELECT 'T7a own-workspace files visible', count(*) = 2
FROM files WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T7b cross-tenant file invisible', count(*) = 0
FROM files WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal ctx: T5 revoked suite-token-a earlier in this run — re-activate it
-- (as staff ctx, then switch to portal ctx) so the portal branch is testable.
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);
UPDATE portal_links SET status = 'active'
WHERE token_hash = encode(sha256('suite-token-a'::bytea), 'hex');
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T7c portal sees customer_visible only', count(*) = 1 AND bool_and(name = 'kickoff.pdf')
FROM files;

-- portal ctx can never see internal file via primary key
INSERT INTO test_results
SELECT 'T7d portal internal-file pk invisible', count(*) = 0
FROM files WHERE id = 'f2222222-2222-2222-2222-222222222222';

-- portal INSERT blocked (policy WITH CHECK requires staff ctx)
DO $check$
BEGIN
  BEGIN
    INSERT INTO files (workspace_id, project_id, name, object_key, content_type, size_bytes, customer_visible)
    VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'evil', 'evil-key', 'text/plain', 1, true);
    RAISE EXCEPTION 'T7e FAIL: portal file insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;
INSERT INTO test_results
SELECT 'T7e portal insert blocked', count(*) = 0
FROM files WHERE name = 'evil';

-- no-context sees nothing
SELECT set_config('app.portal_token_hash', '', false);
SELECT set_config('app.workspace_id', '', false);
INSERT INTO test_results
SELECT 'T7f no-ctx files invisible', count(*) = 0
FROM files;

-- restore tenant ctx for cleanliness
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);


-- ---------- T8: time_entries (00009) ----------
INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, note)
VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', now() - interval '3 hours', now() - interval '1 hour', 120, 'kickoff call');

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T8b cross-tenant time invisible', count(*) = 0
FROM time_entries WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

INSERT INTO test_results
SELECT 'T8a own-workspace time visible', count(*) = 1
FROM time_entries WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant insert blocked (ctx=2222, row claims ws=1111)
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
DO $check$
BEGIN
  BEGIN
    INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes)
    VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', now(), now() + interval '1 hour', 60);
    RAISE EXCEPTION 'T8c FAIL: cross-tenant time insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- no-context sees nothing
SELECT set_config('app.workspace_id', '', false);
INSERT INTO test_results
SELECT 'T8d no-ctx time invisible', count(*) = 0
FROM time_entries;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);


-- ---------- T9: workspace_integrations + sync log (00010) ----------
INSERT INTO test_results
SELECT 'T9a no-ctx integrations invisible', count(*) = 0
FROM workspace_integrations;
INSERT INTO test_results
SELECT 'T9b no-ctx sync log invisible', count(*) = 0
FROM integration_sync_log;

-- staff ctx sees its own rows (seed one row per ws first)
INSERT INTO workspace_integrations (workspace_id, provider, webhook_secret_enc)
VALUES ('11111111-1111-1111-1111-111111111111', 'salesforce', '\x00'::bytea);
INSERT INTO integration_sync_log (workspace_id, provider, external_id)
VALUES ('11111111-1111-1111-1111-111111111111', 'salesforce', 'test-opp-1');

INSERT INTO test_results
SELECT 'T9c own-ws integrations visible', count(*) = 1
FROM workspace_integrations WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T9d cross-ws integrations invisible', count(*) = 0
FROM workspace_integrations WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
INSERT INTO test_results
SELECT 'T9e cross-ws sync log invisible', count(*) = 0
FROM integration_sync_log WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- cross-tenant insert blocked
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
DO $check$
BEGIN
  BEGIN
    INSERT INTO workspace_integrations (workspace_id, provider, webhook_secret_enc)
    VALUES ('11111111-1111-1111-1111-111111111111', 'salesforce', '\x00'::bytea);
    RAISE EXCEPTION 'T9f FAIL: cross-tenant integrations insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);


-- ---------- T10: approvals (00011) ----------
INSERT INTO approvals (workspace_id, project_id, title, requested_by)
VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Kickoff scope sign-off', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'),
  ('11111111-1111-1111-1111-111111111111', '66666666-6666-6666-6666-666666666666', 'Phase 2 sign-off', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa');

-- staff ctx sees it
INSERT INTO test_results
SELECT 'T10a own-ws approvals visible', count(*) = 2
FROM approvals WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T10b cross-tenant approval invisible', count(*) = 0
FROM approvals WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal ctx (active link 'suite-token-a' -> project 5555): sees it
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T10c portal sees approval for its project', count(*) = 1
FROM approvals WHERE project_id = '55555555-5555-5555-5555-555555555555';

-- portal sees NOTHING for project 6666 (no link) even though same ws
INSERT INTO test_results
SELECT 'T10d portal blocked from unlinked project approval', count(*) = 0
FROM approvals WHERE project_id = '66666666-6666-6666-6666-666666666666';

-- portal INSERT blocked (WITH CHECK staff-only)
DO $check$
BEGIN
  BEGIN
    INSERT INTO approvals (workspace_id, project_id, title)
    VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'forged');
    RAISE EXCEPTION 'T10e FAIL: portal approval insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;

-- no-context sees nothing
SELECT set_config('app.portal_token_hash', '', false);
INSERT INTO test_results
SELECT 'T10f no-ctx approval invisible', count(*) = 0
FROM approvals;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- T11: csat (00012) ----------
-- staff decide the linked approval first (CSAT rides decided approvals)
UPDATE approvals SET status = 'approved', decided_at = now()
WHERE project_id = '55555555-5555-5555-5555-555555555555';

-- own-ws csat visible (seed one under staff ctx)
INSERT INTO csat_responses (workspace_id, approval_id, project_id, contact_id, score, comment)
VALUES ('11111111-1111-1111-1111-111111111111',
        (SELECT id FROM approvals WHERE project_id = '55555555-5555-5555-5555-555555555555'),
        '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444', 5, 'great');
INSERT INTO test_results
SELECT 'T11a own-ws csat visible', count(*) = 1
FROM csat_responses WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T11b cross-tenant csat invisible', count(*) = 0
FROM csat_responses WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal ctx: insert allowed for own link (decided approval, matching contact)
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
DO $check$
DECLARE v_id uuid;
BEGIN
  INSERT INTO csat_responses (workspace_id, approval_id, project_id, contact_id, score)
  VALUES ('11111111-1111-1111-1111-111111111111',
          (SELECT id FROM approvals WHERE project_id = '66666666-6666-6666-6666-666666666666'),
          '66666666-6666-6666-6666-666666666666', '44444444-4444-4444-4444-444444444444', 3)
  RETURNING id INTO v_id;
  RAISE EXCEPTION 'T11c FAIL: portal csat insert for UNLINKED project NOT blocked';
EXCEPTION
  WHEN insufficient_privilege THEN NULL;
END
$check$;

-- unique: second response for same approval rejected
DO $check$
DECLARE v_id uuid;
BEGIN
  INSERT INTO csat_responses (workspace_id, approval_id, project_id, contact_id, score)
  VALUES ('11111111-1111-1111-1111-111111111111',
          (SELECT id FROM approvals WHERE project_id = '55555555-5555-5555-5555-555555555555'),
          '55555555-5555-5555-5555-555555555555', '44444444-4444-4444-4444-444444444444', 4);
  RAISE EXCEPTION 'T11d FAIL: duplicate csat for one approval NOT blocked';
EXCEPTION
  WHEN unique_violation THEN NULL;
END
$check$;

-- escalation fn: creates one open task; second call for same project skipped;
-- wrong-project returns NULL. Run under portal ctx (SECURITY DEFINER).
DO $check$
DECLARE v1 uuid; v2 uuid;
BEGIN
  SELECT csat_escalate('55555555-5555-5555-5555-555555555555'::uuid, 2::smallint, 'upset') INTO v1;
  IF v1 IS NULL THEN RAISE EXCEPTION 'T11e FAIL: csat_escalate returned NULL on first call'; END IF;
  SELECT csat_escalate('55555555-5555-5555-5555-555555555555'::uuid, 2::smallint, 'more upset') INTO v2;
  IF v2 IS NOT NULL THEN RAISE EXCEPTION 'T11f FAIL: second escalation NOT deduped'; END IF;
  SELECT csat_escalate('99999999-9999-9999-9999-999999999999'::uuid, 2::smallint, 'ghost') INTO v2;
  IF v2 IS NOT NULL THEN RAISE EXCEPTION 'T11g FAIL: ghost project escalation NOT NULL'; END IF;
END
$check$;

-- no-context sees nothing
SELECT set_config('app.portal_token_hash', '', false);
INSERT INTO test_results
SELECT 'T11h no-ctx csat invisible', count(*) = 0
FROM csat_responses;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- T12: docs (00013) ----------
-- staff seed: doc + v1 on linked project, and one on UNLINKED 6666
INSERT INTO docs (workspace_id, project_id, title, customer_visible, latest_version, created_by)
VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'SOW draft', true, 2, 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'),
  ('11111111-1111-1111-1111-111111111111', '66666666-6666-6666-6666-666666666666', 'Internal notes', false, 1, 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa');
INSERT INTO doc_versions (workspace_id, doc_id, version, content_md)
SELECT d.workspace_id, d.id, 1, 'v1 content' FROM docs d
UNION ALL
SELECT d.workspace_id, d.id, 2, 'v2 content' FROM docs d WHERE d.latest_version = 2;

INSERT INTO test_results
SELECT 'T12a own-ws docs visible', count(*) = 2
FROM docs WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

-- cross-tenant invisible
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T12b cross-tenant docs invisible', count(*) = 0
FROM docs WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal ctx: linked+visible only (SOW draft), not internal notes
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T12c portal sees customer-visible doc', count(*) = 1
FROM docs WHERE customer_visible;

INSERT INTO test_results
SELECT 'T12d portal doc versions scoped', count(*) = 2
FROM doc_versions;

-- portal INSERT blocked
DO $check$
BEGIN
  BEGIN
    INSERT INTO docs (workspace_id, project_id, title)
    VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'forged');
    RAISE EXCEPTION 'T12e FAIL: portal doc insert NOT blocked';
  EXCEPTION
    WHEN insufficient_privilege THEN NULL;
  END;
END
$check$;

-- no-context sees nothing
SELECT set_config('app.portal_token_hash', '', false);
INSERT INTO test_results
SELECT 'T12f no-ctx docs invisible', count(*) = 0
FROM docs;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- T13: forms (00014) ----------
-- staff seed: published form on 5555, unpublished on 5555, published on unlinked 6666
INSERT INTO forms (workspace_id, project_id, title, fields, published)
VALUES
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Onboarding intake',
   '[{"key":"company_size","label":"Company size","type":"text","required":true}]'::jsonb, true),
  ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Draft form', '[]'::jsonb, false),
  ('11111111-1111-1111-1111-111111111111', '66666666-6666-6666-6666-666666666666', 'Other project form', '[]'::jsonb, true);
INSERT INTO form_responses (workspace_id, form_id, contact_id, answers)
SELECT '11111111-1111-1111-1111-111111111111', f.id, '44444444-4444-4444-4444-444444444444',
       '{"company_size":"500"}'::jsonb
FROM forms f WHERE f.title = 'Onboarding intake';

INSERT INTO test_results
SELECT 'T13a own-ws forms visible', count(*) = 3
FROM forms WHERE workspace_id = '11111111-1111-1111-1111-111111111111';

SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T13b cross-tenant forms invisible', count(*) = 0
FROM forms WHERE workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal: sees ONLY the published linked form
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T13c portal sees published linked form', count(*) = 1
FROM forms WHERE published;

-- portal can't reach unpublished/unlinked forms: the INSERT..SELECT
-- sources them through forms' RLS (invisible → 0 rows inserted, no
-- error), so assert NO ROW landed rather than a 42501.
INSERT INTO form_responses (workspace_id, form_id, contact_id, answers)
SELECT '11111111-1111-1111-1111-111111111111', f.id, '44444444-4444-4444-4444-444444444444', '{}'::jsonb
FROM forms f WHERE f.title IN ('Draft form', 'Other project form');
INSERT INTO test_results
SELECT 'T13d portal cannot respond to invisible forms', count(*) = 0
FROM form_responses fr
JOIN forms f ON f.id = fr.form_id
WHERE f.title IN ('Draft form', 'Other project form');

-- portal duplicate response blocked (unique) — delete the seeded row first? No:
-- the seed row blocks a second one; exercise via DO block catching unique_violation
DO $check$
DECLARE v_id uuid;
BEGIN
  INSERT INTO form_responses (workspace_id, form_id, contact_id, answers)
  SELECT '11111111-1111-1111-1111-111111111111', f.id, '44444444-4444-4444-4444-444444444444', '{}'::jsonb
  FROM forms f WHERE f.title = 'Onboarding intake'
  RETURNING id INTO v_id;
  RAISE EXCEPTION 'T13f FAIL: duplicate form response NOT blocked';
EXCEPTION
  WHEN unique_violation THEN NULL;
END
$check$;

-- portal can read its own responses (paired read policy)
INSERT INTO test_results
SELECT 'T13g portal reads own responses', count(*) = 1
FROM form_responses;

-- no-context sees nothing
SELECT set_config('app.portal_token_hash', '', false);
INSERT INTO test_results
SELECT 'T13h no-ctx forms invisible', count(*) = 0
FROM forms;
INSERT INTO test_results
SELECT 'T13i no-ctx responses invisible', count(*) = 0
FROM form_responses;
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- T14: search (00016) ----------
-- trigram indexes exist on all four searchable columns
INSERT INTO test_results
SELECT 'T14a trigram indexes present', count(*) = 4
FROM pg_indexes
WHERE indexname IN ('idx_projects_name_trgm','idx_tasks_title_trgm',
                    'idx_docs_title_trgm','idx_files_name_trgm');

-- staff search (ws ctx): finds the project by prefix match
INSERT INTO test_results
SELECT 'T14b staff search finds own project',
       count(*) = 1
FROM (
    SELECT p.name FROM projects p
    WHERE p.deleted_at IS NULL AND p.name % 'Adobe Onboardng'
      AND p.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
) sub;

-- cross-tenant: the same query under wsB sees nothing of wsA
SELECT set_config('app.workspace_id', '22222222-2222-2222-2222-222222222222', false);
INSERT INTO test_results
SELECT 'T14c cross-tenant search empty', count(*) = 0
FROM projects p
WHERE p.deleted_at IS NULL AND p.name % 'Apollo'
  AND p.workspace_id = '11111111-1111-1111-1111-111111111111';
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- portal search scope: internal-only rows never match even with a
-- perfect term (RLS: portal sees only its project + customer_visible)
SELECT set_config('app.workspace_id', '', false);
SELECT set_config('app.portal_token_hash', encode(sha256('suite-token-a'::bytea), 'hex'), false);
INSERT INTO test_results
SELECT 'T14d portal search cannot see unlinked project',
       count(*) = 0
FROM projects p
WHERE p.name % 'Adobe Phase 2' OR p.id = '66666666-6666-6666-6666-666666666666';
INSERT INTO test_results
SELECT 'T14e portal search sees linked project',
       count(*) = 1
FROM projects p
WHERE p.id = '55555555-5555-5555-5555-555555555555';
SELECT set_config('app.portal_token_hash', '', false);
SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false);

-- ---------- verdict ----------
DO $$
DECLARE failed int; r record;
BEGIN
  SELECT count(*) INTO failed FROM test_results WHERE NOT ok;
  IF failed > 0 THEN
    FOR r IN SELECT name FROM test_results WHERE NOT ok LOOP
      RAISE NOTICE 'FAILED CHECK: %', r.name;
    END LOOP;
    RAISE EXCEPTION 'RLS suite: % of % checks FAILED — names above', failed, (SELECT count(*) FROM test_results);
  END IF;
  RAISE NOTICE 'RLS suite: all % checks passed', (SELECT count(*) FROM test_results);
END $$;
