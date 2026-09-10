-- +goose Up
-- Workspace-scoped settings (issue #45, spec §16): Slack webhook for
-- notifications v1. 1:1 with workspaces; tenant RLS like every data table.

CREATE TABLE workspace_settings (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    slack_webhook_url TEXT,
    notify_task_completed BOOLEAN NOT NULL DEFAULT true,
    notify_project_created BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE workspace_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workspace_settings
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE workspace_settings TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON workspace_settings;
DROP TABLE IF EXISTS workspace_settings;
