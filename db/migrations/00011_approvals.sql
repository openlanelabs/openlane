-- +goose Up
-- Portal approvals v1 (issue #58, spec §7.4): staff request, customer
-- Approves / Requests-changes with a comment. The approval object is
-- first-class; the milestone it refers to is a free-form title until
-- full milestones/Gantt land in P1.

CREATE TABLE approvals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    description TEXT,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','approved','changes_requested')),
    requested_by UUID REFERENCES users(id),
    decided_by UUID REFERENCES contacts(id),
    decision_comment TEXT,
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX approvals_project_idx ON approvals(project_id) WHERE deleted_at IS NULL;
CREATE INDEX approvals_pending_idx ON approvals(workspace_id, project_id)
    WHERE deleted_at IS NULL AND status = 'pending';

ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approvals
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
        OR (
            -- portal reader: active link scoped to this approval's project
            approvals.project_id IN (
                SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), ''))
            )
        )
    )
    WITH CHECK (
        -- portal ctx can never create or rewrite approvals: staff-only writes
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

-- approval events ride the Slack notify plumbing (§16): requested to
-- the team when staff creates, decided back when the customer acts.
ALTER TABLE workspace_settings
    ADD COLUMN IF NOT EXISTS notify_approvals BOOLEAN NOT NULL DEFAULT true;
GRANT SELECT, UPDATE ON TABLE workspace_settings TO openlane_app;

-- portal may UPDATE (decide) only within its linked projects — the
-- same shape as tasks' portal_scope_update (00002). The status CHECK
-- constraint + handler keep transitions sane.
-- +goose StatementBegin
CREATE POLICY portal_scope_update ON approvals FOR UPDATE
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
    )
    WITH CHECK (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
    );
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE approvals TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS portal_scope_update ON approvals;
DROP POLICY IF EXISTS tenant_isolation ON approvals;
DROP TABLE IF EXISTS approvals;
ALTER TABLE workspace_settings DROP COLUMN IF EXISTS notify_approvals;
