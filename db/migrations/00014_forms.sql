-- Forms v1 (§9/§272): definition (fields jsonb) + responses. Required
-- fields are ENFORCED server-side at submit (the Rocketlane gap the
-- spec calls out). Conditional logic, prefill, e-sign: P1+.

-- +goose Up
CREATE TABLE forms (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (title <> '' AND char_length(title) <= 255),
    description TEXT,
    fields JSONB NOT NULL, -- [{key,label,type,required}] type in text|date|file
    published BOOLEAN NOT NULL DEFAULT true,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE TABLE form_responses (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    form_id UUID NOT NULL REFERENCES forms(id) ON DELETE CASCADE,
    contact_id UUID REFERENCES contacts(id),
    answers JSONB NOT NULL,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (form_id, contact_id)
);

ALTER TABLE forms ENABLE ROW LEVEL SECURITY;
ALTER TABLE form_responses ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON forms
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON form_responses
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- portal: read published forms for linked projects (tasks/files/docs
-- visibility line). Never raw portal_links reads in policies — the
-- table is RLS'd; scalar resolvers only (csat #61 shape).
CREATE POLICY portal_scope ON forms
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND published
    );

-- one response per contact per form (UNIQUE enforces it; the policy
-- pins the scope tighter: published form in the link's projects and
-- the link's own contact). form_id check goes through forms' RLS
-- visible rows — a published-but-foreign form is invisible to this
-- ctx, so the join can't be satisfied by cross-tenant ids.
CREATE POLICY portal_scope_insert ON form_responses FOR INSERT
    WITH CHECK (
        form_id IN (SELECT f.id FROM forms f WHERE f.published)
        AND contact_id = portal_link_contact(NULLIF(current_setting('app.portal_token_hash', true), ''))
    );

-- PR #61 paired-read rule: INSERT..RETURNING evaluates SELECT
-- policies on the new row — portal sees its own submissions.
CREATE POLICY portal_scope_read ON form_responses FOR SELECT
    USING (workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), '')));

CREATE INDEX idx_forms_project ON forms(project_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_form_responses_form ON form_responses(form_id, submitted_at DESC);
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON TABLE forms TO openlane_app;
GRANT SELECT, INSERT ON TABLE form_responses TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS portal_scope_read ON form_responses;
DROP POLICY IF EXISTS portal_scope_insert ON form_responses;
DROP POLICY IF EXISTS portal_scope ON forms;
DROP POLICY IF EXISTS tenant_isolation ON form_responses;
DROP POLICY IF EXISTS tenant_isolation ON forms;
DROP TABLE IF EXISTS form_responses;
DROP TABLE IF EXISTS forms;
