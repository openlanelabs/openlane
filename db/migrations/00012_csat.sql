-- CSAT v1 (§14, §218): one response per approval, asked at decision time.
-- In P0 approvals are the milestone gate; the widget rides the decide flow.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE csat_responses (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    approval_id UUID NOT NULL UNIQUE REFERENCES approvals(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    contact_id UUID REFERENCES contacts(id),
    score SMALLINT NOT NULL CHECK (score BETWEEN 1 AND 5),
    comment TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (comment IS NULL OR char_length(comment) <= 2000)
);

ALTER TABLE csat_responses ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON csat_responses
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- portal (magic-link ctx): read none; submit exactly once, only for
-- approvals in the link's projects that the customer just decided.
-- Scalar = resolvers (portal_link_workspace shape from 00002), NOT
-- IN (SELECT srf()): set-returning resolvers inside a WITH CHECK
-- evaluate against the wrong GUC snapshot under prepared statements
-- (pgx extended protocol) and always reject — scalar comparisons
-- read the tx-local GUC correctly. Caught by the integration test;
-- the unprepared RLS suite never sees it.
CREATE OR REPLACE FUNCTION portal_link_project(p_token_hash text)
RETURNS UUID
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public AS $$
    SELECT pl.project_id
    FROM portal_links pl
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
      AND pl.status = 'active'
      AND pl.expires_at > now()
      AND pl.deleted_at IS NULL
$$;

CREATE OR REPLACE FUNCTION portal_link_contact(p_token_hash text)
RETURNS UUID
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public AS $$
    SELECT pl.contact_id
    FROM portal_links pl
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
      AND pl.status = 'active'
      AND pl.expires_at > now()
      AND pl.deleted_at IS NULL
$$;

CREATE POLICY portal_scope_insert ON csat_responses FOR INSERT
    WITH CHECK (
        workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), ''))
        AND project_id = portal_link_project(NULLIF(current_setting('app.portal_token_hash', true), ''))
        AND contact_id = portal_link_contact(NULLIF(current_setting('app.portal_token_hash', true), ''))
    );

-- portal reads its own submitted responses (INSERT ... RETURNING
-- evaluates SELECT policies on the inserted row — without this, the
-- rating insert 42501s under the portal ctx even when the WITH CHECK
-- passes). Scope: the link's workspace, same resolver as audit's
-- portal_scope_insert but readable.
CREATE POLICY portal_scope_read ON csat_responses FOR SELECT
    USING (workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), '')));

CREATE INDEX idx_csat_project ON csat_responses(project_id, created_at);
CREATE INDEX idx_csat_ws_score ON csat_responses(workspace_id, score);
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT ON TABLE csat_responses TO openlane_app;

-- Low-score escalation (§358): the portal session cannot INSERT tasks by
-- design (RLS); a SECURITY DEFINER function lets it, keeping the door
-- narrow: one open escalation per (project, score-band), staff-visible task.
CREATE OR REPLACE FUNCTION csat_escalate(
    p_project UUID, p_score SMALLINT, p_comment TEXT
) RETURNS UUID
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_ws UUID; v_owner UUID; v_id UUID;
BEGIN
    SELECT workspace_id, owner_id INTO v_ws, v_owner FROM projects WHERE id = p_project;
    IF v_ws IS NULL THEN RETURN NULL; END IF;
    -- one open escalation per project per low score
    IF EXISTS (SELECT 1 FROM tasks t
               WHERE t.project_id = p_project AND t.owner_type = 'internal'
                 AND t.status <> 'done' AND t.deleted_at IS NULL
                 AND t.title LIKE 'CSAT escalation:%') THEN
        RETURN NULL;
    END IF;
    INSERT INTO tasks (workspace_id, project_id, title, description_md, owner_type,
                       required, customer_visible, created_by)
    VALUES (v_ws, p_project,
            'CSAT escalation: score ' || p_score,
            'Customer rated ' || p_score || '/5' ||
            COALESCE(E'\nComment: ' || p_comment, '') ||
            E'\n\nFollow up with the customer, then resolve this task.',
            'internal', true, false, NULL)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$;
GRANT EXECUTE ON FUNCTION csat_escalate(UUID, SMALLINT, TEXT) TO openlane_app;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS tenant_isolation ON csat_responses;
DROP POLICY IF EXISTS portal_scope_read ON csat_responses;
DROP POLICY IF EXISTS portal_scope_insert ON csat_responses;
DROP FUNCTION IF EXISTS portal_contact_ids(text);
DROP FUNCTION IF EXISTS portal_link_project(text);
DROP FUNCTION IF EXISTS portal_link_contact(text);
DROP TABLE IF EXISTS csat_responses;
-- +goose StatementEnd
