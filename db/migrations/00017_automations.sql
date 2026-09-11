-- Automations v1 (§408): when/condition/action rules with unlimited
-- runs + history (the anti-Rocketlane edge). P0 triggers ride the same
-- events notifyEvent already fires; actions: slack_message (settings
-- webhook) and create_task. Visual builder/dry-run/replay: P1 web UI.

-- +goose Up
CREATE TABLE automations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name <> '' AND char_length(name) <= 255),
    -- event name exactly as notifyEvent emits it
    trigger_event TEXT NOT NULL CHECK (trigger_event IN (
        'task.completed','approval.requested','approval.decided',
        'csat.submitted','project.created','project.created_from_template')),
    -- optional condition: {"project_id": "..."} — run only for that project
    condition JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(condition) = 'object'),
    -- {"type":"slack_message"} | {"type":"create_task","project_id":"...","title":"...","customer_visible":false}
    action JSONB NOT NULL CHECK (jsonb_typeof(action) = 'object'),
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

-- run history: one row per execution attempt outcome (§408 "run
-- history"; replay re-fires the action — P1 UI calls the same engine)
CREATE TABLE automation_runs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    automation_id UUID NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    event TEXT NOT NULL,
    event_ref TEXT,          -- id of the entity behind the event, when known
    status TEXT NOT NULL CHECK (status IN ('completed','failed','skipped')),
    detail TEXT,             -- failure reason / skip reason / action summary
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE automations ENABLE ROW LEVEL SECURITY;
ALTER TABLE automation_runs ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON automations
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON automation_runs
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

CREATE INDEX idx_automations_ws_trigger ON automations(workspace_id, trigger_event) WHERE deleted_at IS NULL;
CREATE INDEX idx_automation_runs_auto ON automation_runs(automation_id, created_at DESC);
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE automations TO openlane_app;
GRANT SELECT, INSERT ON TABLE automation_runs TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON automation_runs;
DROP POLICY IF EXISTS tenant_isolation ON automations;
DROP TABLE IF EXISTS automation_runs;
DROP TABLE IF EXISTS automations;
